package executor

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hiclaw/hiclaw-controller/internal/credprovider"
)

const (
	nacosAuthTypeNone  = "none"
	nacosAuthTypeNacos = "nacos"
	nacosAuthTypeSTS   = "sts-hiclaw"
)

// nacosCredential abstracts all authentication logic for NacosAIClient.
// NacosAIClient never touches credential state directly — it only calls
// Refresh before sending a request and Apply to inject headers.
type nacosCredential interface {
	// Refresh ensures the credential is valid (re-login or STS refresh as needed).
	Refresh(ctx context.Context) error
	// Apply injects authentication headers into an outbound request.
	Apply(req *http.Request)
}

// ── nacosNoneCredential ───────────────────────────────────────────────────

type nacosNoneCredential struct{}

func (nacosNoneCredential) Refresh(_ context.Context) error { return nil }
func (nacosNoneCredential) Apply(_ *http.Request)           {}

// ── nacosUserPassCredential ───────────────────────────────────────────────

type nacosUserPassCredential struct {
	serverAddr       string
	username         string
	password         string
	httpClient       *http.Client
	mu               sync.Mutex
	accessToken      string
	tokenExpireAt    time.Time
	authLoginVersion string
}

func (c *nacosUserPassCredential) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" {
		if c.tokenExpireAt.IsZero() || time.Now().Add(5*time.Second).Before(c.tokenExpireAt) {
			return nil
		}
	}
	return c.login(ctx)
}

func (c *nacosUserPassCredential) Apply(req *http.Request) {
	c.mu.Lock()
	tok := c.accessToken
	c.mu.Unlock()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

func (c *nacosUserPassCredential) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)

	tryV3 := c.authLoginVersion == "" || c.authLoginVersion == "v3"
	if tryV3 {
		ok, err := c.tryLogin(ctx, fmt.Sprintf("http://%s/nacos/v3/auth/user/login", c.serverAddr), form)
		if err == nil && ok {
			c.authLoginVersion = "v3"
			return nil
		}
	}

	ok, err := c.tryLogin(ctx, fmt.Sprintf("http://%s/nacos/v1/auth/login", c.serverAddr), form)
	if err != nil {
		return err
	}
	if ok {
		c.authLoginVersion = "v1"
		return nil
	}
	return fmt.Errorf("login failed with both v3 and v1 auth endpoints")
}

func (c *nacosUserPassCredential) tryLogin(ctx context.Context, loginURL string, form url.Values) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	return c.applyLoginResponse(body), nil
}

func (c *nacosUserPassCredential) applyLoginResponse(body []byte) bool {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}
	if data, ok := result["data"].(map[string]interface{}); ok {
		return c.applyLoginMap(data)
	}
	return c.applyLoginMap(result)
}

func (c *nacosUserPassCredential) applyLoginMap(data map[string]interface{}) bool {
	token, ok := data["accessToken"].(string)
	if !ok || token == "" {
		return false
	}
	c.accessToken = token

	var ttlSeconds int64
	switch value := data["tokenTtl"].(type) {
	case float64:
		ttlSeconds = int64(value)
	case int64:
		ttlSeconds = value
	case int:
		ttlSeconds = int64(value)
	}
	if ttlSeconds > 0 {
		c.tokenExpireAt = time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	} else {
		c.tokenExpireAt = time.Time{}
	}
	return true
}

// ── nacosSTSCredential ────────────────────────────────────────────────────

type nacosSTSCredential struct {
	namespace string
	tm        *credprovider.TokenManager
	mu        sync.RWMutex
	cached    *credprovider.IssueResponse
}

func newNacosSTSCredential(namespace string, client credprovider.Client) *nacosSTSCredential {
	tm := credprovider.NewTokenManager(client, credprovider.IssueRequest{
		SessionName: "hiclaw-nacos-" + namespace,
		Entries: []credprovider.AccessEntry{
			{
				Service: credprovider.ServiceAIRegistry,
				Scope: credprovider.AccessScope{
					NamespaceID: namespace,
					Resources:   []string{"agentSpec/*", "skill/*"},
				},
			},
		},
	})
	return &nacosSTSCredential{namespace: namespace, tm: tm}
}

func (c *nacosSTSCredential) Refresh(ctx context.Context) error {
	tok, err := c.tm.Token(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cached = tok
	c.mu.Unlock()
	return nil
}

func (c *nacosSTSCredential) Apply(req *http.Request) {
	c.mu.RLock()
	tok := c.cached
	c.mu.RUnlock()
	if tok == nil {
		return
	}

	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	group := "DEFAULT_GROUP"

	signData := c.namespace + "+" + group + "+" + timestamp
	if c.namespace == "" {
		signData = timestamp
	}

	mac := hmac.New(sha1.New, []byte(tok.AccessKeySecret))
	mac.Write([]byte(signData))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req.Header.Set("Spas-AccessKey", tok.AccessKeyID)
	req.Header.Set("Spas-SecurityToken", tok.SecurityToken)
	req.Header.Set("Timestamp", timestamp)
	req.Header.Set("Spas-Signature", signature)
}
