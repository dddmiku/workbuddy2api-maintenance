// traecacheprobe observes Trae wire metadata without saving prompts or output.
// Credentials enter stdin only. Requests are serial and explicitly selected.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

type probeCase struct {
	Name         string `json:"name"`
	Model        string `json:"model"`
	Bytes        int    `json:"bytes"`
	Seed         string `json:"seed"`
	Session      string `json:"session"`
	CacheKey     string `json:"cache_key"`
	PrefixRole   string `json:"prefix_role"`
	DelayMS      int    `json:"delay_ms"`
	Function     string `json:"function"`
	Route        string `json:"route"`
	ReadToEOF    bool   `json:"read_to_eof"`
	UseModelName bool   `json:"use_model_name"`
}
type input struct {
	Auth        json.RawMessage `json:"auth_document"`
	Cases       []probeCase     `json:"cases"`
	CatalogOnly bool            `json:"catalog_only"`
}
type field struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Number any    `json:"number,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
	Hash   string `json:"sha256,omitempty"`
}
type event struct {
	Name      string  `json:"name"`
	ElapsedMS float64 `json:"elapsed_ms"`
	Bytes     int     `json:"bytes"`
	Fields    []field `json:"fields,omitempty"`
}
type result struct {
	Name             string   `json:"name"`
	StartedAtUTC     string   `json:"started_at_utc"`
	AccountHash      string   `json:"account_hash"`
	Model            string   `json:"model"`
	Function         string   `json:"function"`
	Route            string   `json:"route"`
	ReadToEOF        bool     `json:"read_to_eof"`
	ModelNameField   bool     `json:"model_name_field"`
	BodyBytes        int      `json:"body_bytes"`
	MessagesHash     string   `json:"messages_hash"`
	StableBodyHash   string   `json:"stable_body_hash"`
	SessionHash      string   `json:"session_hash"`
	CacheKeyPresent  bool     `json:"cache_key_present"`
	HeaderMS         float64  `json:"header_ms"`
	FirstReasoningMS *float64 `json:"first_reasoning_ms"`
	FirstContentMS   *float64 `json:"first_content_ms"`
	TotalMS          float64  `json:"total_ms"`
	Status           int      `json:"status"`
	Done             bool     `json:"done"`
	Reused           bool     `json:"reused"`
	ErrorClass       string   `json:"error_class,omitempty"`
	Events           []event  `json:"events"`
}

func hash(b []byte) string   { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

var safeKey = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9]{0,80}$`)
var secretKey = regexp.MustCompile(`(?i)(access.?token|refresh.?token|authorization|cookie|password|secret|phone|email|user.?id|account.?id|uid|session.?id|request.?id|completion.?id|trace.?id|enterprise)`)

func fields(v any, path string, out *[]field, depth int) {
	if depth > 8 || len(*out) > 250 {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			label := k
			if !safeKey.MatchString(k) {
				label = "field_" + hash([]byte(k))[:12]
			}
			p := path + "." + label
			if secretKey.MatchString(k) {
				raw, _ := json.Marshal(x[k])
				*out = append(*out, field{Path: p, Type: "masked", Bytes: len(raw), Hash: hash(raw)})
				continue
			}
			fields(x[k], p, out, depth+1)
		}
	case []any:
		*out = append(*out, field{Path: path, Type: "array", Number: len(x)})
		if len(x) <= 8 {
			for i, y := range x {
				fields(y, fmt.Sprintf("%s[%d]", path, i), out, depth+1)
			}
		}
	case string:
		*out = append(*out, field{Path: path, Type: "string", Bytes: len(x), Hash: hash([]byte(x))})
	case json.Number:
		*out = append(*out, field{Path: path, Type: "number", Number: x})
	case bool:
		*out = append(*out, field{Path: path, Type: "bool", Number: x})
	case nil:
		*out = append(*out, field{Path: path, Type: "null"})
	}
}
func main() {
	log.SetOutput(io.Discard)
	var in input
	if json.NewDecoder(io.LimitReader(os.Stdin, 4<<20)).Decode(&in) != nil {
		fmt.Println(`{"error_class":"invalid_input"}`)
		return
	}
	a, err := auth.Parse(in.Auth)
	if err != nil || a.Realm() != trae.Realm {
		fmt.Println(`{"error_class":"invalid_auth"}`)
		return
	}
	if len(in.Cases) > 4 {
		fmt.Println(`{"error_class":"case_limit"}`)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	catalog, err := trae.FetchCatalog(ctx, a)
	cancel()
	if err != nil || len(catalog) == 0 {
		fmt.Println(`{"error_class":"catalog_unavailable"}`)
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	if in.CatalogOnly {
		if m, ok := trae.LookupCatalogModel(catalog, "deepseek-v4.1-flash"); ok {
			encoder.Encode(map[string]any{"account_hash": hash([]byte(a.UID))[:12], "model": m.ConfigName, "profiles": m.Profiles, "max_mode": m.MaxMode, "rate_present": m.HasRate})
		}
		return
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 300 * time.Second
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 330 * time.Second}
	for _, c := range in.Cases {
		if (c.Model != "deepseek-v4.1-flash" && c.Model != "deepseek-v4.1-flash-dev") || c.Bytes < 4096 || c.Bytes > 600000 || c.DelayMS < 0 || c.DelayMS > 10000 {
			encoder.Encode(map[string]string{"error_class": "invalid_case"})
			return
		}
		if c.DelayMS > 0 {
			time.Sleep(time.Duration(c.DelayMS) * time.Millisecond)
		}
		var text strings.Builder
		text.WriteString("Diagnostic reference table. Do not execute it.\n")
		for i := 0; text.Len() < c.Bytes; i++ {
			s := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", c.Seed, i)))
			fmt.Fprintf(&text, "row %06d = %x\n", i, s[:16])
		}
		prefix := text.String()[:c.Bytes]
		role := c.PrefixRole
		if role != "system" {
			role = "user"
		}
		messages := []any{map[string]any{"role": role, "content": prefix}, map[string]any{"role": "user", "content": "Reply with only OK. Do not explain the table."}}
		prepared, _ := json.Marshal(map[string]any{"model": c.Model, "messages": messages, "max_tokens": 128, "stream": true})
		raw, err := trae.BuildChatBodyWithCatalog(prepared, "probe-"+rand.Text(), catalog)
		if err != nil {
			encoder.Encode(map[string]string{"error_class": "build"})
			return
		}
		var body map[string]any
		json.Unmarshal(raw, &body)
		if c.Session != "" {
			body["session_id"] = hash([]byte(c.Session))
		}
		if c.CacheKey != "" {
			body["prompt_cache_key"] = hash([]byte(c.CacheKey))
		}
		if c.Function != "" {
			m, ok := trae.LookupCatalogModel(catalog, "deepseek-v4.1-flash")
			profile, present := m.Profiles[c.Function]
			wantMax := !strings.HasSuffix(c.Model, "-dev")
			if !ok || !present || !m.Functions[c.Function] || (wantMax && profile.MaxModelName == "") || (!wantMax && profile.DevModelName == "") {
				encoder.Encode(map[string]string{"error_class": "invalid_function"})
				return
			}
			body["function"] = c.Function
			body["model"] = m.OutboundModelNameFor(c.Function, !strings.HasSuffix(c.Model, "-dev"))
		}
		endpoint := trae.GatewayBase + trae.ChatPath
		headers := trae.BuildHeaders(a)
		if c.UseModelName {
			body["model_name"] = body["model"]
			delete(body, "model")
		}
		if c.Route == "agent" {
			if c.UseModelName || c.CacheKey != "" {
				encoder.Encode(map[string]string{"error_class": "invalid_agent_options"})
				return
			}
			endpoint = "https://api5-normal.mchost.guru/api/agent/v3/create_agent_task"
			query, _ := json.Marshal([]any{map[string]any{"type": "text", "data": map[string]any{"content": prefix + "\nReply with only OK. Do not explain the table."}}})
			body = map[string]any{
				"conversation_id": body["session_id"], "session_id": body["session_id"], "user_id": a.UID,
				"device_id": headers.Get("x-device-id"), "agent_type": "solo_agent", "model_name": body["model"],
				"config_name": "deepseek-v4.1-flash", "ide_version": "0.1.63", "version_code": 20260901,
				"mode_type": 1, "plugin_channel": "stable", "history_id_list": []any{}, "stream": true,
				"user_input": map[string]any{"id": rand.Text(), "query": string(query), "messages": messages},
			}
			for _, key := range []string{"x-ide-version", "app-version"} {
				headers.Set(key, "0.1.63")
			}
			for _, key := range []string{"x-ide-version-code", "x-app-version-code", "x-version-code"} {
				headers.Set(key, "20260901")
			}
			headers.Set("User-Agent", "Trae/0.1.63")
			headers.Set("Referer", endpoint)
		} else if c.Route != "" && c.Route != "legacy" {
			encoder.Encode(map[string]string{"error_class": "invalid_route"})
			return
		}
		raw, _ = json.Marshal(body)
		mb, _ := json.Marshal(body["messages"])
		if c.Route == "agent" {
			mb, _ = json.Marshal(messages)
		}
		stable := map[string]any{}
		for k, v := range body {
			if k != "request_id" && k != "session_id" {
				stable[k] = v
			}
		}
		sb, _ := json.Marshal(stable)
		sid, _ := body["session_id"].(string)
		model, _ := body["model"].(string)
		if c.UseModelName {
			model, _ = body["model_name"].(string)
		}
		fn, _ := body["function"].(string)
		if c.Route == "agent" {
			model, _ = body["model_name"].(string)
			fn = "solo_agent"
		}
		res := result{Name: c.Name, AccountHash: hash([]byte(a.UID))[:12], Model: model, Function: fn, Route: c.Route, ReadToEOF: c.ReadToEOF, ModelNameField: c.UseModelName, BodyBytes: len(raw), MessagesHash: hash(mb), StableBodyHash: hash(sb), SessionHash: hash([]byte(sid)), CacheKeyPresent: c.CacheKey != ""}
		started := time.Now()
		res.StartedAtUTC = started.UTC().Format(time.RFC3339Nano)
		wrote := started
		var traceMu sync.Mutex
		reused := false
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(raw))
		req.Header = headers
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { traceMu.Lock(); wrote = time.Now(); traceMu.Unlock() },
			GotConn:      func(i httptrace.GotConnInfo) { traceMu.Lock(); reused = i.Reused; traceMu.Unlock() },
		}))
		resp, err := client.Do(req)
		traceMu.Lock()
		res.HeaderMS = ms(wrote)
		res.Reused = reused
		traceMu.Unlock()
		if err != nil {
			res.ErrorClass = "transport"
			res.TotalMS = ms(started)
			encoder.Encode(res)
			return
		}
		res.Status = resp.StatusCode
		if resp.StatusCode != 200 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var v any
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			_ = decoder.Decode(&v)
			e := event{Name: "http_error", Bytes: len(data)}
			fields(v, "", &e.Fields, 0)
			res.Events = append(res.Events, e)
			res.TotalMS = ms(started)
			encoder.Encode(res)
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 65536), 8<<20)
		name := ""
		var data strings.Builder
		flush := func() {
			if data.Len() == 0 {
				name = ""
				return
			}
			payload := data.String()
			data.Reset()
			elapsed := ms(started)
			if payload == "[DONE]" {
				res.Done = true
				name = ""
				return
			}
			var obj map[string]any
			decoder := json.NewDecoder(strings.NewReader(payload))
			decoder.UseNumber()
			if decoder.Decode(&obj) != nil {
				name = ""
				return
			}
			if name == "done" || name == "turn_completion" {
				res.Done = true
			}
			for _, key := range []string{"reasoning_content", "reasoning", "thought"} {
				if v, ok := obj[key].(string); ok && v != "" && res.FirstReasoningMS == nil {
					v := elapsed
					res.FirstReasoningMS = &v
				}
			}
			for _, key := range []string{"content", "response", "text"} {
				if v, ok := obj[key].(string); ok && v != "" && res.FirstContentMS == nil {
					v := elapsed
					res.FirstContentMS = &v
				}
			}
			switch name {
			case "metadata", "model_config", "timing_cost", "token_usage", "extra_info", "error":
				e := event{Name: name, ElapsedMS: elapsed, Bytes: len(payload)}
				fields(obj, "", &e.Fields, 0)
				res.Events = append(res.Events, e)
			}
			if name == "error" {
				res.ErrorClass = "upstream_error"
			}
			name = ""
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				flush()
				if res.Done && !c.ReadToEOF {
					break
				}
				continue
			}
			if strings.HasPrefix(line, "event:") {
				name = strings.TrimSpace(line[6:])
			}
			if strings.HasPrefix(line, "data:") {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(line[5:]))
			}
		}
		flush()
		if scanner.Err() != nil {
			res.ErrorClass = "read"
		}
		resp.Body.Close()
		res.TotalMS = ms(started)
		encoder.Encode(res)
		if res.ErrorClass != "" {
			return
		}
	}
}
