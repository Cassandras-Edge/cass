package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type SessionInfo struct {
	SessionID    string `json:"session_id"`
	Status       string `json:"status"`
	Model        string `json:"model"`
	PodIP        string `json:"pod_ip"`
	CreatedAt    string `json:"created_at"`
	LastActivity string `json:"last_activity"`
}

type SessionListResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionRequest struct {
	Model string `json:"model,omitempty"`
	Vault string `json:"vault,omitempty"`
}

type CreateSessionResponse struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status,omitempty"`
	PodIP     string `json:"pod_ip"`
}

type APIClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewAPIClient(baseURL, apiKey string) *APIClient {
	return &APIClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *APIClient) ListSessions() ([]SessionInfo, error) {
	data, err := c.request("GET", "/sessions", nil)
	if err != nil {
		return nil, err
	}
	var resp SessionListResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	return resp.Sessions, nil
}

func (c *APIClient) CreateSession(req SessionRequest) (*CreateSessionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	data, err := c.request("POST", "/sessions", body)
	if err != nil {
		return nil, err
	}
	var resp CreateSessionResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}

func (c *APIClient) DeleteSession(id string) error {
	_, err := c.request("DELETE", "/sessions/"+id, nil)
	return err
}

func (c *APIClient) request(method, path string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}
