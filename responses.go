package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// streamState 保存单个流式请求里跨分片需要的信息，按宿主 RequestID 索引。
type streamState struct {
	reasoningIDs map[int]string      // output_index → reasoning 条目 id
	chatIndexes  map[int]map[int]int // choice 下标 → (上游 tool_calls.index → 从 0 连续编号后的 index)
	fixes        int
	touched      time.Time
}

const streamStateTTL = 30 * time.Minute

var (
	// maxStreamStates 限制同时跟踪的流数量；流中断没收到终态事件时，状态靠 TTL 与淘汰回收。
	maxStreamStates = 4096

	statesMu sync.Mutex
	states   = make(map[string]*streamState)

	nativeSeen sync.Map
)

// noteNative 在首次发现上游已原生提供某项修补内容时记一条日志（每项每进程一次），
// 方便确认 CPA 官方修复后关闭对应开关或卸载插件。修补本身只在字段缺失时才发生，
// 上游修复后自动变成空操作，不需要人工干预。
func noteNative(feature string) {
	if _, loaded := nativeSeen.LoadOrStore(feature, true); !loaded {
		logf("upstream-native feature=%s：上游已原生提供，本项修补自动跳过", feature)
	}
}

func newStreamState() *streamState {
	return &streamState{reasoningIDs: map[int]string{}, chatIndexes: map[int]map[int]int{}}
}

// withState 在锁内对 requestID 的状态执行 fn。requestID 为空时使用一次性状态。
func withState(requestID string, fn func(*streamState)) {
	if requestID == "" {
		fn(newStreamState())
		return
	}
	statesMu.Lock()
	defer statesMu.Unlock()
	now := time.Now()
	st := states[requestID]
	if st == nil {
		if len(states) >= maxStreamStates {
			evictStates(now)
		}
		st = newStreamState()
		states[requestID] = st
	}
	st.touched = now
	fn(st)
}

// evictStates 先清掉超过 TTL 的状态；仍然满额时淘汰最久未访问的一条，保证内存有上界。
// 调用方需持有 statesMu。
func evictStates(now time.Time) {
	for id, old := range states {
		if now.Sub(old.touched) > streamStateTTL {
			delete(states, id)
		}
	}
	for len(states) >= maxStreamStates {
		oldestID := ""
		var oldest time.Time
		for id, st := range states {
			if oldestID == "" || st.touched.Before(oldest) {
				oldestID, oldest = id, st.touched
			}
		}
		delete(states, oldestID)
	}
}

func dropState(requestID string) {
	if requestID == "" {
		return
	}
	statesMu.Lock()
	delete(states, requestID)
	statesMu.Unlock()
}

// reasoningItemID 返回 output_index 对应的 reasoning 条目 id。
// 没见过 output_item.added 时回退到宿主转换器的默认命名 item_<index>。
func (st *streamState) reasoningItemID(index int) string {
	if id := st.reasoningIDs[index]; id != "" {
		return id
	}
	return "item_" + strconv.Itoa(index)
}

// rewriteDataLines 对分片里每个 JSON 负载调用 fn：既支持 SSE 的 "data: {...}" 行，
// 也支持整块就是一个 JSON 对象（chat 分片由宿主在写出时才加 data: 前缀）。
// 未改动的行按原字节保留；不是合法 JSON 的负载不交给 fn，改写结果不合法时丢弃改写。
func rewriteDataLines(body []byte, fn func(payload []byte) ([]byte, bool)) ([]byte, bool) {
	safe := func(payload []byte) ([]byte, bool) {
		if !gjson.ValidBytes(payload) {
			return payload, false
		}
		out, changed := fn(payload)
		if !changed || !json.Valid(out) {
			return payload, false
		}
		return out, true
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if out, changed := safe(trimmed); changed {
			return out, true
		}
		return body, false
	}
	lines := bytes.Split(body, []byte("\n"))
	changedAny := false
	for i, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		start := bytes.IndexByte(line, '{')
		if start < 0 {
			continue
		}
		end := len(line)
		if bytes.HasSuffix(line, []byte("\r")) {
			end--
		}
		out, changed := safe(line[start:end])
		if !changed {
			continue
		}
		rebuilt := make([]byte, 0, start+len(out)+1)
		rebuilt = append(rebuilt, line[:start]...)
		rebuilt = append(rebuilt, out...)
		rebuilt = append(rebuilt, line[end:]...)
		lines[i] = rebuilt
		changedAny = true
	}
	if !changedAny {
		return body, false
	}
	return bytes.Join(lines, []byte("\n")), true
}

// patcher 在一段 JSON 上累积 sjson 修改并计数。
type patcher struct {
	out   []byte
	fixes int
}

func (p *patcher) set(path string, value any) {
	if out, err := sjson.SetBytes(p.out, path, value); err == nil {
		p.out = out
		p.fixes++
	}
}

func isTerminalResponsesEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

// patchResponsesChunk 补齐单个 Responses 流分片里缺失的必填字段。
func patchResponsesChunk(requestID string, body []byte) ([]byte, bool) {
	terminal := false
	fixes := 0
	var out []byte
	var changed bool
	withState(requestID, func(st *streamState) {
		out, changed = rewriteDataLines(body, func(payload []byte) ([]byte, bool) {
			root := gjson.ParseBytes(payload)
			if isTerminalResponsesEvent(root.Get("type").String()) {
				terminal = true
			}
			patched, n := patchResponsesEvent(st, root, payload)
			fixes += n
			return patched, n > 0
		})
		st.fixes += fixes
		if terminal && st.fixes > 0 {
			logf("responses-fix stream request=%s fixes=%d", requestID, st.fixes)
		}
	})
	if terminal {
		dropState(requestID)
	}
	return out, changed
}

// patchResponsesEvent 按 AI SDK（@ai-sdk/openai）的流式事件 schema 补字段：
//   - response.created / in_progress / 终态事件：response.created_at
//   - response.reasoning_summary_*：item_id、summary_index
//   - response.output_item.added / done：function_call 的 status，以及缺失的条目 id
func patchResponsesEvent(st *streamState, root gjson.Result, payload []byte) ([]byte, int) {
	p := &patcher{out: payload}
	eventType := root.Get("type").String()
	switch {
	case eventType == "response.created" || eventType == "response.in_progress" || isTerminalResponsesEvent(eventType):
		if root.Get("response").IsObject() {
			if !root.Get("response.created_at").Exists() {
				p.set("response.created_at", time.Now().Unix())
			} else if eventType == "response.created" {
				noteNative("response.created_at")
			}
			for i, item := range root.Get("response.output").Array() {
				patchOutputItem(p, fmt.Sprintf("response.output.%d", i), item, i, true)
			}
		}
	case eventType == "response.output_item.added" || eventType == "response.output_item.done":
		index := int(root.Get("output_index").Int())
		item := root.Get("item")
		if item.Get("type").String() == "reasoning" {
			if id := item.Get("id").String(); id != "" {
				st.reasoningIDs[index] = id
			} else {
				p.set("item.id", st.reasoningItemID(index))
			}
		}
		patchOutputItem(p, "item", item, index, eventType == "response.output_item.done")
	case strings.HasPrefix(eventType, "response.reasoning_summary_"):
		index := int(root.Get("output_index").Int())
		if root.Get("item_id").String() == "" {
			p.set("item_id", st.reasoningItemID(index))
		} else {
			noteNative("reasoning_summary.item_id")
		}
		if !root.Get("summary_index").Exists() {
			p.set("summary_index", 0)
		} else {
			noteNative("reasoning_summary.summary_index")
		}
	}
	return p.out, p.fixes
}

// patchOutputItem 补齐单个输出条目的 id / status，以及 message 文本片段的 annotations。
// done 表示条目已结束。
func patchOutputItem(p *patcher, prefix string, item gjson.Result, index int, done bool) {
	status := "in_progress"
	if done {
		status = "completed"
	}
	switch item.Get("type").String() {
	case "function_call", "custom_tool_call":
		if item.Get("id").String() == "" {
			p.set(prefix+".id", firstNonEmpty(item.Get("call_id").String(), "fc_"+strconv.Itoa(index)))
		}
		if !item.Get("status").Exists() {
			p.set(prefix+".status", status)
		} else {
			noteNative("function_call.status")
		}
	case "message":
		if item.Get("id").String() == "" {
			p.set(prefix+".id", "msg_"+strconv.Itoa(index))
		}
		if !item.Get("status").Exists() {
			p.set(prefix+".status", status)
		}
		for ci, part := range item.Get("content").Array() {
			if part.Get("type").String() != "output_text" {
				continue
			}
			if part.Get("annotations").IsArray() {
				noteNative("message.annotations")
			} else {
				p.set(fmt.Sprintf("%s.content.%d.annotations", prefix, ci), []any{})
			}
		}
	case "reasoning":
		if item.Get("id").String() == "" {
			p.set(prefix+".id", "rs_"+strconv.Itoa(index))
		}
	}
}

// patchResponsesObject 补齐非流式 Responses 结果的 created_at 与输出条目 id/status。
func patchResponsesObject(body []byte) ([]byte, int) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() || !root.Get("output").IsArray() {
		return body, 0
	}
	p := &patcher{out: body}
	if !root.Get("created_at").Exists() {
		p.set("created_at", time.Now().Unix())
	}
	for i, item := range root.Get("output").Array() {
		patchOutputItem(p, fmt.Sprintf("output.%d", i), item, i, true)
	}
	return p.out, p.fixes
}

// normalizeChatChunk 把 chat/completions 流里的 tool_calls[].index 映射成从 0 开始的连续编号。
// 宿主转换器直接用 Interactions 步骤下标做 index，前面有思考步骤时第一个工具调用就是 1。
// 映射依赖同一请求跨分片的状态，拿不到 RequestID 时宁可不改，避免不同分片各自从 0 编号撞号。
func normalizeChatChunk(requestID string, body []byte) ([]byte, bool) {
	if requestID == "" {
		return body, false
	}
	finished := false
	var out []byte
	var changed bool
	withState(requestID, func(st *streamState) {
		out, changed = rewriteDataLines(body, func(payload []byte) ([]byte, bool) {
			root := gjson.ParseBytes(payload)
			p := &patcher{out: payload}
			for ci, choice := range root.Get("choices").Array() {
				if fr := choice.Get("finish_reason"); fr.Exists() && fr.Type != gjson.Null {
					finished = true
				}
				for ti, call := range choice.Get("delta.tool_calls").Array() {
					index := call.Get("index")
					if !index.Exists() {
						continue
					}
					original := int(index.Int())
					indexes := st.chatIndexes[ci]
					if indexes == nil {
						indexes = map[int]int{}
						st.chatIndexes[ci] = indexes
					}
					mapped, ok := indexes[original]
					if !ok {
						mapped = len(indexes)
						indexes[original] = mapped
					}
					if mapped != original {
						p.set(fmt.Sprintf("choices.%d.delta.tool_calls.%d.index", ci, ti), mapped)
					}
				}
			}
			return p.out, p.fixes > 0
		})
	})
	if finished {
		dropState(requestID)
	}
	return out, changed
}
