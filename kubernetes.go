package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var errNotConfigured = errors.New("credentials not configured")

type credentials struct {
	Username        string `json:"username"`
	Token           string `json:"token,omitempty"`
	DockerNamespace string `json:"dockerNamespace"`
}
type secretStore interface {
	Get(context.Context) (credentials, error)
	Put(context.Context, credentials) error
	Delete(context.Context) error
}
type kubernetesSecretStore struct {
	namespace, name, api, token string
	client                      *http.Client
	initErr                     error
}

func newKubernetesSecretStore(namespace, name string, fallback *http.Client) *kubernetesSecretStore {
	s := &kubernetesSecretStore{namespace: namespace, name: name, api: "https://kubernetes.default.svc", client: fallback}
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		s.initErr = fmt.Errorf("service account token: %w", err)
		return s
	}
	s.token = strings.TrimSpace(string(b))
	ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		s.initErr = fmt.Errorf("cluster CA: %w", err)
		return s
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		s.initErr = errors.New("invalid cluster CA")
	} else {
		s.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	}
	return s
}
func (s *kubernetesSecretStore) endpoint() string {
	return s.api + "/api/v1/namespaces/" + url.PathEscape(s.namespace) + "/secrets/" + url.PathEscape(s.name)
}
func (s *kubernetesSecretStore) request(ctx context.Context, method, endpoint string, body any) (*http.Response, error) {
	if s.initErr != nil {
		return nil, s.initErr
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.client.Do(req)
}
func (s *kubernetesSecretStore) Get(ctx context.Context) (credentials, error) {
	resp, err := s.request(ctx, "GET", s.endpoint(), nil)
	if err != nil {
		return credentials{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return credentials{}, errNotConfigured
	}
	if resp.StatusCode != 200 {
		return credentials{}, fmt.Errorf("Kubernetes API returned %s", resp.Status)
	}
	var v struct {
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return credentials{}, err
	}
	decode := func(key string) string { b, _ := base64.StdEncoding.DecodeString(v.Data[key]); return string(b) }
	c := credentials{Username: decode("username"), Token: decode("token"), DockerNamespace: decode("dockerNamespace")}
	if c.Username == "" || c.Token == "" {
		return credentials{}, errNotConfigured
	}
	if c.DockerNamespace == "" {
		c.DockerNamespace = c.Username
	}
	return c, nil
}
func (s *kubernetesSecretStore) Put(ctx context.Context, c credentials) error {
	data := map[string]string{"username": base64.StdEncoding.EncodeToString([]byte(c.Username)), "token": base64.StdEncoding.EncodeToString([]byte(c.Token)), "dockerNamespace": base64.StdEncoding.EncodeToString([]byte(c.DockerNamespace))}
	body := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": s.name, "namespace": s.namespace}, "type": "Opaque", "data": data}
	method := "POST"
	endpoint := strings.TrimSuffix(s.endpoint(), "/"+url.PathEscape(s.name))
	if _, err := s.Get(ctx); err == nil {
		method, endpoint = "PUT", s.endpoint()
	} else if !errors.Is(err, errNotConfigured) {
		return err
	}
	resp, err := s.request(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("Kubernetes API returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
func (s *kubernetesSecretStore) Delete(ctx context.Context) error {
	resp, err := s.request(ctx, "DELETE", s.endpoint(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return errNotConfigured
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("Kubernetes API returned %s", resp.Status)
	}
	return nil
}
