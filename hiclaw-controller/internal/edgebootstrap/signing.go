package edgebootstrap

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	DefaultControllerName = "default"
	SecretNamePrefix      = "agentteams-edge-bootstrap-signing-"
	SecretPurpose         = "edge-bootstrap-signing"
	TokenType             = "agentteams.edge-bootstrap"
	DefaultJWTTTL         = 7 * 24 * time.Hour
)

var (
	ErrUnavailable  = errors.New("edge bootstrap signing secret unavailable")
	ErrInvalidToken = errors.New("invalid edge bootstrap jwt")
)

type Service struct {
	client         kubernetes.Interface
	namespace      string
	controllerName string
}

type KeyMaterial struct {
	KID     string
	HMACKey []byte
}

type Claims struct {
	Type       string `json:"typ"`
	Issuer     string `json:"iss"`
	Audience   string `json:"aud"`
	Subject    string `json:"sub"`
	WorkerUUID string `json:"workerUuid"`
	IssuedAt   int64  `json:"iat"`
	NotBefore  int64  `json:"nbf"`
	ExpiresAt  int64  `json:"exp"`
	JTI        string `json:"jti"`
}

type SignedToken struct {
	Token     string
	ExpiresAt time.Time
}

func New(client kubernetes.Interface, namespace, controllerName string) *Service {
	return &Service{
		client:         client,
		namespace:      namespace,
		controllerName: NormalizeControllerName(controllerName),
	}
}

func NormalizeControllerName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return DefaultControllerName
	}
	return trimmed
}

func SecretName(controllerName string) string {
	return SecretNamePrefix + NormalizeControllerName(controllerName)
}

func (s *Service) ControllerName() string {
	return s.controllerName
}

func (s *Service) Ensure(ctx context.Context) error {
	if s == nil || s.client == nil {
		return ErrUnavailable
	}
	name := SecretName(s.controllerName)
	_, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: get signing secret: %v", ErrUnavailable, err)
	}

	kidRandom, err := randomBytes(12)
	if err != nil {
		return fmt.Errorf("%w: generate kid: %v", ErrUnavailable, err)
	}
	hmacKey, err := randomBytes(32)
	if err != nil {
		return fmt.Errorf("%w: generate hmac key: %v", ErrUnavailable, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/component":  "controller",
				v1beta1.LabelController:        s.controllerName,
				"agentteams.io/secret-purpose": SecretPurpose,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"kid":     []byte("kid-" + base64.RawURLEncoding.EncodeToString(kidRandom)),
			"hmacKey": hmacKey,
		},
	}
	if _, err := s.client.CoreV1().Secrets(s.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("%w: create signing secret: %v", ErrUnavailable, err)
	}
	return nil
}

func (s *Service) Load(ctx context.Context) (*KeyMaterial, error) {
	if s == nil || s.client == nil {
		return nil, ErrUnavailable
	}
	secret, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, SecretName(s.controllerName), metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: load signing secret: %v", ErrUnavailable, err)
	}
	kid := strings.TrimSpace(string(secret.Data["kid"]))
	hmacKey := secret.Data["hmacKey"]
	if kid == "" || len(hmacKey) != 32 {
		return nil, fmt.Errorf("%w: invalid signing secret", ErrUnavailable)
	}
	keyCopy := append([]byte(nil), hmacKey...)
	return &KeyMaterial{KID: kid, HMACKey: keyCopy}, nil
}

func (s *Service) Sign(ctx context.Context, workerUUID string, ttl time.Duration, now time.Time) (*SignedToken, error) {
	workerUUID = strings.TrimSpace(workerUUID)
	if workerUUID == "" {
		return nil, fmt.Errorf("%w: worker uuid is required", ErrInvalidToken)
	}
	if ttl <= 0 {
		ttl = DefaultJWTTTL
	}
	key, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	expiresAt := now.Add(ttl)
	jtiBytes, err := randomBytes(16)
	if err != nil {
		return nil, fmt.Errorf("%w: generate jti: %v", ErrUnavailable, err)
	}
	header := map[string]string{
		"alg": "HS256",
		"typ": "JWT",
		"kid": key.KID,
	}
	claims := Claims{
		Type:       TokenType,
		Issuer:     issuer(s.controllerName),
		Audience:   audience(s.controllerName),
		Subject:    workerUUID,
		WorkerUUID: workerUUID,
		IssuedAt:   now.Unix(),
		NotBefore:  now.Unix(),
		ExpiresAt:  expiresAt.Unix(),
		JTI:        base64.RawURLEncoding.EncodeToString(jtiBytes),
	}
	unsigned, err := encodeSegments(header, claims)
	if err != nil {
		return nil, err
	}
	signature := sign(unsigned, key.HMACKey)
	return &SignedToken{
		Token:     unsigned + "." + base64.RawURLEncoding.EncodeToString(signature),
		ExpiresAt: expiresAt,
	}, nil
}

func (s *Service) Verify(ctx context.Context, token string, now time.Time) (*Claims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("%w: jwt token is required", ErrInvalidToken)
	}
	key, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: malformed token", ErrInvalidToken)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		KID string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return nil, fmt.Errorf("%w: invalid header", ErrInvalidToken)
	}
	if header.Alg != "HS256" || header.Typ != "JWT" || header.KID != key.KID {
		return nil, fmt.Errorf("%w: header mismatch", ErrInvalidToken)
	}
	unsigned := parts[0] + "." + parts[1]
	expected := sign(unsigned, key.HMACKey)
	actual, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(actual, expected) {
		return nil, fmt.Errorf("%w: signature mismatch", ErrInvalidToken)
	}
	var claims Claims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("%w: invalid claims", ErrInvalidToken)
	}
	if err := s.validateClaims(&claims, now.UTC()); err != nil {
		return nil, err
	}
	return &claims, nil
}

func (s *Service) validateClaims(claims *Claims, now time.Time) error {
	if claims.Type != TokenType ||
		claims.Issuer != issuer(s.controllerName) ||
		claims.Audience != audience(s.controllerName) ||
		claims.WorkerUUID == "" ||
		claims.Subject == "" ||
		claims.WorkerUUID != claims.Subject {
		return fmt.Errorf("%w: claims mismatch", ErrInvalidToken)
	}
	nowUnix := now.Unix()
	if claims.ExpiresAt <= nowUnix {
		return fmt.Errorf("%w: token expired", ErrInvalidToken)
	}
	if claims.NotBefore > nowUnix || claims.IssuedAt > nowUnix {
		return fmt.Errorf("%w: token not yet valid", ErrInvalidToken)
	}
	return nil
}

func issuer(controllerName string) string {
	return "agentteams-controller:" + NormalizeControllerName(controllerName)
}

func audience(controllerName string) string {
	return "agentteams-edge:" + NormalizeControllerName(controllerName)
}

func randomBytes(size int) ([]byte, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func encodeSegments(header map[string]string, claims Claims) (string, error) {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON), nil
}

func decodeSegment(segment string, out any) error {
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func sign(unsigned string, key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	return mac.Sum(nil)
}
