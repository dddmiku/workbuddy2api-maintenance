// traetiming runs the real handler and Trae adapter in an isolated local server.
// Credentials arrive on stdin and never enter argv, files, logs or output.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

type probeCase struct {
	Name           string `json:"name"`
	Model          string `json:"model"`
	InputChars     int    `json:"input_chars"`
	Shape          string `json:"shape"`
	Task           string `json:"task"`
	ConversationID string `json:"conversation_id"`
}
type input struct {
	Auth        json.RawMessage `json:"auth_document"`
	Cases       []probeCase     `json:"cases"`
	WarmCatalog bool            `json:"warm_catalog"`
	Legacy      bool            `json:"legacy"`
}
type result struct {
	Name             string             `json:"name"`
	Model            string             `json:"model"`
	InputChars       int                `json:"input_chars"`
	Shape            string             `json:"shape"`
	Task             string             `json:"task"`
	AccountHash      string             `json:"account_hash"`
	InputHash        string             `json:"input_hash"`
	Events           []trae.TimingEvent `json:"events"`
	FirstByteMS      *float64           `json:"first_byte_ms"`
	FirstContentMS   *float64           `json:"first_content_ms"`
	FirstReasoningMS *float64           `json:"first_reasoning_ms"`
	FirstRoleMS      *float64           `json:"first_role_ms"`
	TotalMS          float64            `json:"total_ms"`
	Status           int                `json:"status"`
	ContentBytes     int                `json:"content_bytes"`
	ReasoningBytes   int                `json:"reasoning_bytes"`
	Done             bool               `json:"done"`
	InputTokens      int                `json:"input_tokens"`
	CachedTokens     *int               `json:"cached_tokens"`
	Credit           *float64           `json:"credit"`
	ErrorClass       string             `json:"error_class,omitempty"`
	ErrorCode        string             `json:"error_code,omitempty"`
}

func millis(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000 }
func first(dst **float64, start time.Time) {
	if *dst == nil {
		v := millis(start)
		*dst = &v
	}
}

func main() {
	out := os.Stdout
	log.SetOutput(io.Discard)
	quiet, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintln(out, `{"error_class":"output_setup"}`)
		return
	}
	defer quiet.Close()
	os.Stdout = quiet
	var in input
	if json.NewDecoder(io.LimitReader(os.Stdin, 8<<20)).Decode(&in) != nil {
		fmt.Fprintln(out, `{"error_class":"input_decode"}`)
		return
	}
	if in.Legacy {
		// Isolated diagnostic process only; never changes deployed routing.
		_ = os.Setenv("WB2API_TRAE_REMOTE", "0")
	}
	a, err := auth.Parse(in.Auth)
	if err != nil || a == nil || a.Realm() != trae.Realm {
		fmt.Fprintln(out, `{"error_class":"invalid_auth_snapshot"}`)
		return
	}
	if in.WarmCatalog {
		_, _ = trae.FetchCatalog(context.Background(), a)
	}
	p := pool.New("")
	p.Add(a)
	p.SetCredits(a.UID, 10000)
	up := upstream.New()
	callerKey := rand.Text()
	h := server.NewHandler(server.Config{Pool: p, Upstream: up, GlobalEnabled: true, APIKey: callerKey})
	var mu sync.Mutex
	var events []trae.TimingEvent
	finished := make(chan struct{}, 1)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { finished <- struct{}{} }()
		ctx := trae.WithTimingObserver(r.Context(), func(e trae.TimingEvent) { mu.Lock(); events = append(events, e); mu.Unlock() })
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer local.Close()
	client := &http.Client{Timeout: 360 * time.Second}
	accountHash := sha256.Sum256([]byte(a.UID))
	encoder := json.NewEncoder(out)
	for _, c := range in.Cases {
		if (c.Model != "glm-5.3" && c.Model != "deepseek-v4.1-flash") || c.InputChars < 0 || c.InputChars > 1000000 {
			encoder.Encode(map[string]string{"error_class": "invalid_case"})
			return
		}
		mu.Lock()
		events = nil
		mu.Unlock()
		prompt := "请只回复 OK，不要分析或解释。"
		if c.InputChars > 0 {
			data := strings.Repeat("case x y z.\n", c.InputChars/12)
			if c.Shape == "unique" {
				var b strings.Builder
				for i := 0; b.Len() < c.InputChars; i++ {
					fmt.Fprintf(&b, "row=%06d x=%08x y=%08x z=%08x\n", i, uint32(i*2654435761), uint32(i*2246822519), uint32(i*3266489917))
				}
				data = b.String()[:c.InputChars]
			}
			prompt = "以下文本仅为输入长度诊断数据，无需执行任何操作：\n" + data + "\n请只回复 OK，不要分析或解释。"
			if c.Task == "review" {
				prompt = "以下是确定性生成的诊断数据：\n" + data + "\n请分析 x/y/z 数列的生成规律，简要解释推导和程序验证步骤。不要调用外部工具。回答不超过80字。"
			}
		}
		body, _ := json.Marshal(map[string]any{"model": "trae:" + c.Model, "messages": []any{map[string]string{"role": "user", "content": prompt}}, "stream": true, "stream_options": map[string]bool{"include_usage": true}, "max_tokens": 128, "conversation_id": c.ConversationID})
		hash := sha256.Sum256([]byte(prompt))
		res := result{Name: c.Name, Model: c.Model, InputChars: len(prompt), Shape: c.Shape, Task: c.Task, AccountHash: hex.EncodeToString(accountHash[:6]), InputHash: hex.EncodeToString(hash[:8])}
		start := time.Now()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, local.URL+"/v1/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+callerKey)
		resp, err := client.Do(req)
		if err != nil {
			res.ErrorClass = "client_transport"
		} else {
			res.Status = resp.StatusCode
			reader := bufio.NewReader(resp.Body)
			for {
				line, rerr := reader.ReadString('\n')
				if len(line) > 0 {
					first(&res.FirstByteMS, start)
				}
				if strings.HasPrefix(line, "data:") {
					payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if payload == "[DONE]" {
						res.Done = true
						break
					}
					var frame struct {
						Usage struct {
							Prompt  int      `json:"prompt_tokens"`
							Credit  *float64 `json:"credit"`
							Details struct {
								Cached *int `json:"cached_tokens"`
							} `json:"prompt_tokens_details"`
						} `json:"usage"`
						Choices []struct {
							Delta struct {
								Content   string `json:"content"`
								Reasoning string `json:"reasoning_content"`
								Role      string `json:"role"`
							} `json:"delta"`
						} `json:"choices"`
						Error struct {
							Code    string `json:"code"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if json.Unmarshal([]byte(payload), &frame) == nil {
						if frame.Usage.Prompt > 0 {
							res.InputTokens = frame.Usage.Prompt
						}
						if frame.Usage.Details.Cached != nil {
							res.CachedTokens = frame.Usage.Details.Cached
						}
						if frame.Usage.Credit != nil {
							res.Credit = frame.Usage.Credit
						}
						if frame.Error.Code != "" || frame.Error.Message != "" {
							res.ErrorClass = "stream_error"
							res.ErrorCode = frame.Error.Code
							if strings.Contains(frame.Error.Message, "首个模型事件超时") {
								res.ErrorCode = "remote_first_event_timeout"
							}
						}
						for _, ch := range frame.Choices {
							if ch.Delta.Role != "" {
								first(&res.FirstRoleMS, start)
							}
							if ch.Delta.Content != "" {
								first(&res.FirstContentMS, start)
								res.ContentBytes += len(ch.Delta.Content)
							}
							if ch.Delta.Reasoning != "" {
								first(&res.FirstReasoningMS, start)
								res.ReasoningBytes += len(ch.Delta.Reasoning)
							}
						}
					}
				}
				if rerr != nil {
					if rerr != io.EOF {
						res.ErrorClass = "client_read"
					}
					break
				}
			}
			_ = resp.Body.Close()
			select {
			case <-finished:
			case <-time.After(15 * time.Second):
				res.ErrorClass = "cleanup_timeout"
			}
		}
		res.TotalMS = millis(start)
		mu.Lock()
		res.Events = append([]trae.TimingEvent(nil), events...)
		mu.Unlock()
		encoder.Encode(res)
	}
}
