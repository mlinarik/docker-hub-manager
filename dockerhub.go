package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type hubClient struct {
	base        string
	client      *http.Client
	credentials credentials
	bearer      string
}
type page[T any] struct {
	Count    int    `json:"count"`
	Next     string `json:"next"`
	Previous string `json:"previous"`
	Results  []T    `json:"results"`
}
type repository struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Description string `json:"description"`
	IsPrivate   bool   `json:"is_private"`
	PullCount   int64  `json:"pull_count"`
	LastUpdated string `json:"last_updated"`
	StorageSize *int64 `json:"storage_size"`
}
type tag struct {
	Name          string `json:"name"`
	FullSize      int64  `json:"full_size"`
	LastUpdated   string `json:"last_updated"`
	TagLastPushed string `json:"tag_last_pushed"`
	TagLastPulled string `json:"tag_last_pulled"`
	Images        []struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Digest       string `json:"digest"`
		Size         int64  `json:"size"`
	} `json:"images"`
}

func (h *hubClient) authenticate(ctx context.Context) (string, error) {
	b, _ := json.Marshal(map[string]string{"identifier": h.credentials.Username, "secret": h.credentials.Token})
	req, _ := http.NewRequestWithContext(ctx, "POST", h.base+"/v2/auth/token", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Docker Hub unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", apiError(resp)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken != "" {
		return out.AccessToken, nil
	}
	if out.Token != "" {
		return out.Token, nil
	}
	return "", fmt.Errorf("Docker Hub returned no access token")
}
func (h *hubClient) doJSON(ctx context.Context, method, endpoint string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, method, h.base+endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+h.bearer)
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("Docker Hub unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
func (h *hubClient) repositories(ctx context.Context, pageNumber, size int, query string) (page[repository], error) {
	var out page[repository]
	q := url.Values{"page": {strconv.Itoa(pageNumber)}, "page_size": {strconv.Itoa(size)}, "ordering": {"-last_updated"}}
	if query != "" {
		q.Set("name", query)
	}
	path := "/v2/namespaces/" + url.PathEscape(h.credentials.DockerNamespace) + "/repositories?" + q.Encode()
	return out, h.doJSON(ctx, "GET", path, &out)
}
func (h *hubClient) tags(ctx context.Context, repo string, pageNumber, size int) (page[tag], error) {
	var out page[tag]
	q := url.Values{"page": {strconv.Itoa(pageNumber)}, "page_size": {strconv.Itoa(size)}}
	path := "/v2/namespaces/" + url.PathEscape(h.credentials.DockerNamespace) + "/repositories/" + url.PathEscape(repo) + "/tags?" + q.Encode()
	return out, h.doJSON(ctx, "GET", path, &out)
}
func (h *hubClient) allRepositories(ctx context.Context, query string, maxPages int) ([]repository, error) {
	var all []repository
	for p := 1; p <= maxPages; p++ {
		pg, err := h.repositories(ctx, p, 100, query)
		if err != nil {
			return all, err
		}
		all = append(all, pg.Results...)
		if pg.Next == "" || len(pg.Results) == 0 {
			break
		}
	}
	return all, nil
}
func (h *hubClient) allTags(ctx context.Context, repo string, maxPages int) ([]tag, error) {
	var all []tag
	for p := 1; p <= maxPages; p++ {
		pg, err := h.tags(ctx, repo, p, 100)
		if err != nil {
			return all, err
		}
		all = append(all, pg.Results...)
		if pg.Next == "" || len(pg.Results) == 0 {
			break
		}
	}
	return all, nil
}
func (h *hubClient) deleteTag(ctx context.Context, repo, tagName string) error {
	path := "/v2/namespaces/" + url.PathEscape(h.credentials.DockerNamespace) + "/repositories/" + url.PathEscape(repo) + "/tags/" + url.PathEscape(tagName)
	return h.doJSON(ctx, "DELETE", path, nil)
}
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var v struct {
		Detail  any    `json:"detail"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(b, &v)
	msg := v.Message
	if msg == "" {
		if s, ok := v.Detail.(string); ok {
			msg = s
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("Docker Hub: %s", msg)
}
