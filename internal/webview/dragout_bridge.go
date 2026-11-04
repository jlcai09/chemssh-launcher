package webview

import "encoding/json"

// dragOutRequest is sent through the binding in the top-level window, even
// when the file page lives in an iframe. Marker remains opaque to the host.
type dragOutRequest struct {
	ID      string   `json:"id"`
	Session string   `json:"session"`
	Paths   []string `json:"paths"`
	Marker  string   `json:"marker"`
}

// dragOutStagingScript reports actual staging, rather than the armed drag.
func dragOutStagingScript(id string, caching bool) string {
	result, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Caching bool   `json:"caching"`
	}{ID: id, Caching: caching})
	return "window.dispatchEvent(new CustomEvent('chemssh-native-drag-staging', {detail:" + string(result) + "}))"
}

// dragOutJobScript is independent of gesture completion.
func dragOutJobScript(job dragJob) string {
	result, _ := json.Marshal(job)
	return "window.dispatchEvent(new CustomEvent('chemssh-native-drag-job', {detail:" + string(result) + "}))"
}

func dragOutCompletionScript(id, status string, ok bool) string {
	result, _ := json.Marshal(struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		OK     bool   `json:"ok"`
	}{ID: id, Status: status, OK: ok})
	return "window.dispatchEvent(new CustomEvent('chemssh-native-drag-finished', {detail:" + string(result) + "}))"
}
