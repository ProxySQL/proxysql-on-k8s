/*
Copyright 2026 ProxySQL.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package rdsclient calls the AWS Query-compatible API exposed by the
// ProxySQL AWS and web plugins. It deliberately implements only the operations
// the operator needs to reconcile an already-bootstrapped proxy.
package rdsclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const apiVersion = "2014-10-31"

// Credentials are the operator-owned signing credentials installed during
// bootstrap. They are never returned by the plugin API.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// Client is scoped to one ProxySQL replica.
type Client struct {
	endpoint    string
	region      string
	credentials Credentials
	httpClient  *http.Client
	now         func() time.Time
}

func New(endpoint, region string, credentials Credentials, httpClient *http.Client) (*Client, error) {
	if endpoint == "" || region == "" || credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" {
		return nil, fmt.Errorf("RDS endpoint, region, and signing credentials are required")
	}
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return nil, fmt.Errorf("invalid RDS endpoint: %w", err)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{endpoint: endpoint, region: region, credentials: credentials, httpClient: httpClient, now: time.Now}, nil
}

// Call sends one AWS Query request and decodes its result into out. A nil out
// is valid for operations whose modeled response is empty.
func (c *Client) Call(ctx context.Context, action string, values url.Values, out any) error {
	if values == nil {
		values = url.Values{}
	}
	values.Set("Action", action)
	values.Set("Version", apiVersion)
	body := values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", action, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sign(req, []byte(body), c.region, c.credentials, c.now().UTC())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed: %w", action, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", action, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var response errorResponse
		if xml.Unmarshal(payload, &response) == nil && response.Error.Code != "" {
			return &APIError{StatusCode: resp.StatusCode, Code: response.Error.Code, Message: response.Error.Message}
		}
		return &APIError{StatusCode: resp.StatusCode, Code: "HTTPError", Message: http.StatusText(resp.StatusCode)}
	}
	if out != nil && len(payload) != 0 {
		if err := xml.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("decode %s response: %w", action, err)
		}
	}
	return nil
}

type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("RDS API %s (HTTP %d)", e.Code, e.StatusCode)
	}
	return fmt.Sprintf("RDS API %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
}

type errorResponse struct {
	Error struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

func sign(req *http.Request, payload []byte, region string, credentials Credentials, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Date", amzDate)

	signedHeaders := "content-type;host;x-amz-date"
	canonicalHeaders := "content-type:" + strings.TrimSpace(req.Header.Get("Content-Type")) + "\n" +
		"host:" + req.Host + "\n" +
		"x-amz-date:" + amzDate + "\n"
	canonicalRequest := req.Method + "\n" + canonicalURI(req.URL.EscapedPath()) + "\n" +
		canonicalQuery(req.URL.Query()) + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash
	scope := date + "/" + region + "/rds/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))

	key := hmacSHA256([]byte("AWS4"+credentials.SecretAccessKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "rds")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+credentials.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

func canonicalQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		items := append([]string(nil), values[key]...)
		sort.Strings(items)
		for _, value := range items {
			parts = append(parts, awsEscape(key)+"="+awsEscape(value))
		}
	}
	return strings.Join(parts, "&")
}

func awsEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

func setBool(values url.Values, name string, value bool) {
	values.Set(name, strconv.FormatBool(value))
}

func setInt(values url.Values, name string, value int) {
	values.Set(name, strconv.Itoa(value))
}
