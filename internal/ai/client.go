package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultModel is used when MES_AI_MODEL is not set.
const DefaultModel = "claude-sonnet-5-5"

// Config configures the Anthropic Messages API client.
type Config struct {
	APIKey     string
	Model      string
	BaseURL    string // default https://api.anthropic.com
	Timeout    time.Duration
	MaxTokens  int
	HTTPClient *http.Client
	// Retries is how many times an overloaded or rate-limited request is
	// retried. Default 2.
	Retries int
}

// Client drafts notes with Claude through the Messages API. It uses the
// plain HTTP API rather than an SDK, which keeps the dependency list at one.
type Client struct {
	apiKey    string
	model     string
	baseURL   string
	maxTokens int
	retries   int
	http      *http.Client
	sleep     func(context.Context, time.Duration) error
}

// New builds a client. An empty API key is an error: callers treat drafting
// as switched off instead of constructing a client that always fails.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("ai: no API key configured")
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	} else if cfg.Retries == 0 {
		cfg.Retries = 2
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{
		apiKey: cfg.APIKey, model: cfg.Model, baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		maxTokens: cfg.MaxTokens, retries: cfg.Retries, http: hc, sleep: sleepCtx,
	}, nil
}

func (c *Client) Model() string { return c.model }

type messagesRequest struct {
	Model       string         `json:"model"`
	MaxTokens   int            `json:"max_tokens"`
	Temperature float64        `json:"temperature"`
	System      string         `json:"system"`
	Messages    []message      `json:"messages"`
	Tools       []tool         `json:"tools"`
	ToolChoice  map[string]any `json:"tool_choice"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type messagesResponse struct {
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type errorResponse struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// RequestBody builds the JSON sent to the provider. Exported for tests
// that assert what leaves the building.
func (c *Client) RequestBody(req Request) ([]byte, error) {
	return json.Marshal(messagesRequest{
		Model:       c.model,
		MaxTokens:   c.maxTokens,
		Temperature: 0,
		System:      SystemPrompt,
		Messages:    []message{{Role: "user", Content: UserMessage(req)}},
		Tools: []tool{{
			Name:        ToolName,
			Description: "Record the clinician's dictation organised into the five sections of a consultation note, with anything that needs checking listed in flags.",
			InputSchema: toolSchema,
		}},
		ToolChoice: map[string]any{"type": "tool", "name": ToolName},
	})
}

// DraftNote sends one dictation and returns the draft.
func (c *Client) DraftNote(ctx context.Context, req Request) (*Result, error) {
	body, err := c.RequestBody(req)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		res, retryAfter, err := c.send(ctx, body)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if retryAfter < 0 || attempt == c.retries {
			break
		}
		if err := c.sleep(ctx, retryAfter); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// send makes one request. retryAfter is negative when the error is not
// worth retrying.
func (c *Client) send(ctx context.Context, body []byte) (*Result, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, -1, ctx.Err()
		}
		return nil, time.Second, fmt.Errorf("ai: request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, time.Second, fmt.Errorf("ai: reading reply: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var er errorResponse
		_ = json.Unmarshal(raw, &er)
		apiErr := &APIError{Status: resp.StatusCode, Type: er.Error.Type, Message: er.Error.Message}
		if apiErr.Message == "" {
			apiErr.Message = http.StatusText(resp.StatusCode)
		}
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
			return nil, retryDelay(resp.Header.Get("retry-after")), apiErr
		}
		return nil, -1, apiErr
	}

	var mr messagesResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return nil, -1, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if mr.StopReason == "max_tokens" {
		return nil, -1, ErrTruncated
	}
	for _, block := range mr.Content {
		if block.Type == "tool_use" && block.Name == ToolName {
			draft, discarded, err := parseDraft(block.Input)
			if err != nil {
				return nil, -1, err
			}
			return &Result{Draft: draft, Model: mr.Model, InputTokens: mr.Usage.InputTokens,
				OutputTokens: mr.Usage.OutputTokens, Discarded: discarded}, 0, nil
		}
	}
	return nil, -1, fmt.Errorf("%w: no %s call in the reply", ErrMalformed, ToolName)
}

// retryDelay honours a Retry-After in seconds, within reason.
func retryDelay(header string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && s > 0 {
		d := time.Duration(s) * time.Second
		if d > 10*time.Second {
			d = 10 * time.Second
		}
		return d
	}
	return 2 * time.Second
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
