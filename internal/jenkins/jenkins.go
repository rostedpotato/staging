// Package jenkins triggers builds and reads their status/logs (stdlib only).
package jenkins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base  string
	user  string
	token string
	http  *http.Client
}

func New(base, user, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		user:  user,
		token: token,
		http:  &http.Client{Timeout: 20 * time.Second},
	}
}

// Trigger starts a parameterized build. params typically {ENV, BRANCH|TAG}.
// Returns the queue item URL (from the Location header) used to resolve the build.
func (c *Client) Trigger(ctx context.Context, job string, params map[string]string) (string, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	endpoint := fmt.Sprintf("%s/job/%s/buildWithParameters", c.base, url.PathEscape(job))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.user, c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("jenkins trigger http %d", resp.StatusCode)
	}
	// 201 Created with Location: .../queue/item/<id>/
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("jenkins: no queue Location header")
	}
	return strings.TrimRight(loc, "/"), nil
}

type queueItem struct {
	Cancelled  bool `json:"cancelled"`
	Executable *struct {
		Number int64  `json:"number"`
		URL    string `json:"url"`
	} `json:"executable"`
}

// ResolveBuild polls the queue item until a build number is assigned.
// Returns (0,"",nil) if not started yet; error if cancelled.
func (c *Client) ResolveBuild(ctx context.Context, queueURL string) (int64, string, error) {
	body, err := c.get(ctx, queueURL+"/api/json")
	if err != nil {
		return 0, "", err
	}
	var qi queueItem
	if err := json.Unmarshal(body, &qi); err != nil {
		return 0, "", err
	}
	if qi.Cancelled {
		return 0, "", fmt.Errorf("build cancelled in queue")
	}
	if qi.Executable == nil {
		return 0, "", nil // still waiting
	}
	return qi.Executable.Number, qi.Executable.URL, nil
}

type buildInfo struct {
	Building bool   `json:"building"`
	Result   string `json:"result"` // SUCCESS | FAILURE | ABORTED | null
	Number   int64  `json:"number"`
	URL      string `json:"url"`
}

// BuildStatus reports whether a build is still running and its result.
func (c *Client) BuildStatus(ctx context.Context, job string, number int64) (building bool, result string, err error) {
	u := fmt.Sprintf("%s/job/%s/%d/api/json", c.base, url.PathEscape(job), number)
	body, err := c.get(ctx, u)
	if err != nil {
		return false, "", err
	}
	var bi buildInfo
	if err := json.Unmarshal(body, &bi); err != nil {
		return false, "", err
	}
	return bi.Building, bi.Result, nil
}

// ConsoleText fetches the full console log for a build.
func (c *Client) ConsoleText(ctx context.Context, job string, number int64) (string, error) {
	u := fmt.Sprintf("%s/job/%s/%d/consoleText", c.base, url.PathEscape(job), number)
	body, err := c.get(ctx, u)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("jenkins http %d: %s", resp.StatusCode, u)
	}
	return body, nil
}
