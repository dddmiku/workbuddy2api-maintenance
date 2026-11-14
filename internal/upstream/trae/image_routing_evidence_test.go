package trae

import "testing"

// Historical media still requires legacy: Remote cannot preserve its meaning.
// Text mentioning image-shaped JSON or a data URL is never itself a media block.
func TestImageRoutingUsesStructuredHistory(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		any, fresh bool
	}{
		{"text_only", `{"messages":[{"role":"user","content":"image_url data:image/png;base64,fixture"}]}`, false, false},
		{"text_block", `{"messages":[{"role":"user","content":[{"type":"text","text":"image_url"}]}]}`, false, false},
		{"current_image", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Zg=="}}]}]}`, true, true},
		{"historical_image", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Zg=="}}]},{"role":"assistant","content":"previous answer"},{"role":"user","content":"continue"}]}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if requestHasImages([]byte(tc.body)) != tc.any || freshInputHasImages([]byte(tc.body)) != tc.fresh {
				t.Fatal("structured image routing did not distinguish history from the current turn")
			}
		})
	}
}
