package edgebootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureCreatesSigningSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	service := New(client, "test-ns", "controller-a")

	if err := service.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure signing secret: %v", err)
	}

	secret, err := client.CoreV1().Secrets("test-ns").Get(
		context.Background(), SecretName("controller-a"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get signing secret: %v", err)
	}
	if secret.Labels[v1beta1.LabelController] != "controller-a" {
		t.Fatalf("controller label=%q, want controller-a", secret.Labels[v1beta1.LabelController])
	}
	if secret.Labels["agentteams.io/secret-purpose"] != SecretPurpose {
		t.Fatalf("secret purpose label=%q, want %q", secret.Labels["agentteams.io/secret-purpose"], SecretPurpose)
	}
	if string(secret.Data["kid"]) == "" {
		t.Fatal("kid must be set")
	}
	if len(secret.Data["hmacKey"]) != 32 {
		t.Fatalf("hmac key length=%d, want 32", len(secret.Data["hmacKey"]))
	}
}

func TestEnsureUsesDefaultControllerName(t *testing.T) {
	client := fake.NewSimpleClientset()
	service := New(client, "test-ns", " ")

	if err := service.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure signing secret: %v", err)
	}
	if service.ControllerName() != DefaultControllerName {
		t.Fatalf("controller name=%q, want %q", service.ControllerName(), DefaultControllerName)
	}

	secret, err := client.CoreV1().Secrets("test-ns").Get(
		context.Background(), SecretName(DefaultControllerName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get default signing secret: %v", err)
	}
	if secret.Labels[v1beta1.LabelController] != DefaultControllerName {
		t.Fatalf("controller label=%q, want %q", secret.Labels[v1beta1.LabelController], DefaultControllerName)
	}
}

func TestEnsureKeepsExistingSigningSecret(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName("controller-a"),
			Namespace: "test-ns",
		},
		Data: map[string][]byte{
			"kid":     []byte("kid-existing"),
			"hmacKey": []byte("01234567890123456789012345678901"),
		},
	}
	client := fake.NewSimpleClientset(existing)
	service := New(client, "test-ns", "controller-a")

	if err := service.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure signing secret: %v", err)
	}

	secret, err := client.CoreV1().Secrets("test-ns").Get(
		context.Background(), SecretName("controller-a"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get signing secret: %v", err)
	}
	if string(secret.Data["kid"]) != "kid-existing" {
		t.Fatalf("kid was overwritten: %q", string(secret.Data["kid"]))
	}
}

func TestLoadInvalidSigningSecretReturnsUnavailable(t *testing.T) {
	tests := []struct {
		name string
		data map[string][]byte
	}{
		{
			name: "missing kid",
			data: map[string][]byte{"hmacKey": []byte("01234567890123456789012345678901")},
		},
		{
			name: "invalid hmac key length",
			data: map[string][]byte{"kid": []byte("kid-1"), "hmacKey": []byte("too-short")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      SecretName("controller-a"),
					Namespace: "test-ns",
				},
				Data: tt.data,
			}
			service := New(fake.NewSimpleClientset(secret), "test-ns", "controller-a")

			_, err := service.Load(context.Background())
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("load invalid secret error=%v, want ErrUnavailable", err)
			}
		})
	}
}

func TestSignAndVerifyJWT(t *testing.T) {
	service := New(fake.NewSimpleClientset(), "test-ns", "controller-a")
	if err := service.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure signing secret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()

	signed, err := service.Sign(context.Background(), "uuid-1", time.Hour, now)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	claims, err := service.Verify(context.Background(), signed.Token, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("verify jwt: %v", err)
	}

	if claims.WorkerUUID != "uuid-1" || claims.Subject != "uuid-1" {
		t.Fatalf("worker uuid claims=%q/%q, want uuid-1", claims.WorkerUUID, claims.Subject)
	}
	if claims.Type != TokenType {
		t.Fatalf("claim typ=%q, want %q", claims.Type, TokenType)
	}
	if claims.Issuer != "agentteams-controller:controller-a" {
		t.Fatalf("issuer=%q", claims.Issuer)
	}
	if signed.ExpiresAt != now.Add(time.Hour) {
		t.Fatalf("expiresAt=%s, want %s", signed.ExpiresAt, now.Add(time.Hour))
	}
}

func TestVerifyRejectsWrongKid(t *testing.T) {
	client := fake.NewSimpleClientset()
	service := New(client, "test-ns", "controller-a")
	if err := service.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure signing secret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	signed, err := service.Sign(context.Background(), "uuid-1", time.Hour, now)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}

	secret, err := client.CoreV1().Secrets("test-ns").Get(
		context.Background(), SecretName("controller-a"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get signing secret: %v", err)
	}
	secret.Data["kid"] = []byte("kid-other")
	if _, err := client.CoreV1().Secrets("test-ns").Update(
		context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update signing secret: %v", err)
	}

	_, err = service.Verify(context.Background(), signed.Token, now.Add(time.Minute))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("verify wrong kid error=%v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsExpiredJWT(t *testing.T) {
	service := New(fake.NewSimpleClientset(), "test-ns", "controller-a")
	if err := service.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure signing secret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	signed, err := service.Sign(context.Background(), "uuid-1", time.Minute, now)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}

	_, err = service.Verify(context.Background(), signed.Token, now.Add(2*time.Minute))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("verify expired token error=%v, want ErrInvalidToken", err)
	}
}

func TestLoadMissingSigningSecretReturnsUnavailable(t *testing.T) {
	service := New(fake.NewSimpleClientset(), "test-ns", "controller-a")

	_, err := service.Load(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("load missing secret error=%v, want ErrUnavailable", err)
	}
}
