package service

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WorkerCredentials holds persisted credentials for a worker.
// These are generated once on first creation and reused across retries.
type WorkerCredentials struct {
	MatrixPassword string
	MinIOPassword  string
	GatewayKey     string
	// MatrixToken is the access token returned by the most recent matrix.Login.
	// Persisted so that subsequent RefreshManagerCredentials calls can reuse
	// the cached token instead of issuing a fresh login on every controller
	// reconcile. Without this, every reconcile produced a brand-new token,
	// the controller pushed it into the manager's openclaw.json (shared
	// filesystem mount), and openclaw detected the change → gateway restart
	// → matrix client torn down. May be empty on first boot or when the old
	// token has been invalidated; callers must re-login in that case.
	MatrixToken string
}

// CredentialStore manages worker credential persistence.
type CredentialStore interface {
	Load(ctx context.Context, workerName string) (*WorkerCredentials, error)
	Save(ctx context.Context, workerName string, creds *WorkerCredentials) error
	Delete(ctx context.Context, workerName string) error
	// List returns the names of all workers/managers with stored credentials.
	List(ctx context.Context) ([]string, error)
}

// FileCredentialStore persists credentials as env files (embedded mode).
// Compatible with the existing /data/worker-creds/{name}.env format.
type FileCredentialStore struct {
	Dir string // e.g. /data/worker-creds
}

func (s *FileCredentialStore) Load(_ context.Context, workerName string) (*WorkerCredentials, error) {
	path := filepath.Join(s.Dir, workerName+".env")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open credentials file: %w", err)
	}
	defer f.Close()

	creds := &WorkerCredentials{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v := parseEnvLine(line)
		switch k {
		case "WORKER_PASSWORD":
			creds.MatrixPassword = v
		case "WORKER_MINIO_PASSWORD":
			creds.MinIOPassword = v
		case "WORKER_GATEWAY_KEY":
			creds.GatewayKey = v
		case "WORKER_MATRIX_TOKEN":
			creds.MatrixToken = v
		}
	}
	return creds, scanner.Err()
}

func (s *FileCredentialStore) Save(_ context.Context, workerName string, creds *WorkerCredentials) error {
	if err := os.MkdirAll(s.Dir, 0755); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}
	path := filepath.Join(s.Dir, workerName+".env")
	content := fmt.Sprintf(
		"WORKER_PASSWORD=%q\nWORKER_MINIO_PASSWORD=%q\nWORKER_GATEWAY_KEY=%q\nWORKER_MATRIX_TOKEN=%q\n",
		creds.MatrixPassword, creds.MinIOPassword, creds.GatewayKey, creds.MatrixToken,
	)
	return os.WriteFile(path, []byte(content), 0600)
}

func (s *FileCredentialStore) Delete(_ context.Context, workerName string) error {
	path := filepath.Join(s.Dir, workerName+".env")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *FileCredentialStore) List(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read creds dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".env") {
			names = append(names, strings.TrimSuffix(name, ".env"))
		}
	}
	return names, nil
}

func parseEnvLine(line string) (string, string) {
	idx := strings.IndexByte(line, '=')
	if idx < 0 {
		return line, ""
	}
	k := line[:idx]
	v := line[idx+1:]
	v = strings.Trim(v, `"'`)
	return k, v
}

// GenerateCredentials creates a fresh set of worker credentials.
func GenerateCredentials() (*WorkerCredentials, error) {
	matrixPw, err := generateRandomHex(16)
	if err != nil {
		return nil, fmt.Errorf("generate matrix password: %w", err)
	}
	minioPw, err := generateRandomHex(24)
	if err != nil {
		return nil, fmt.Errorf("generate minio password: %w", err)
	}
	gwKey, err := generateRandomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate gateway key: %w", err)
	}
	return &WorkerCredentials{
		MatrixPassword: matrixPw,
		MinIOPassword:  minioPw,
		GatewayKey:     gwKey,
	}, nil
}

func generateRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SecretCredentialStore persists credentials as K8s Secrets (incluster mode).
// Secret name: hiclaw-creds-{workerName}
type SecretCredentialStore struct {
	Client    kubernetes.Interface
	Namespace string
	// ControllerName identifies this controller instance. Stamped on the
	// credential Secret via hiclaw.io/controller so multi-instance
	// deployments sharing a namespace can filter by owner.
	ControllerName string
	// ResourcePrefix is the tenant prefix used to derive the decorative
	// "app" label on the Secret (via WorkerAppLabel()). Empty falls back
	// to auth.DefaultResourcePrefix — keeps the Secret's "app" value
	// aligned with the Pod and ServiceAccount created for the same worker.
	ResourcePrefix auth.ResourcePrefix
}

func (s *SecretCredentialStore) secretName(workerName string) string {
	return "hiclaw-creds-" + workerName
}

func (s *SecretCredentialStore) Load(ctx context.Context, workerName string) (*WorkerCredentials, error) {
	secret, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, s.secretName(workerName), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get credentials secret: %w", err)
	}
	return &WorkerCredentials{
		MatrixPassword: string(secret.Data["WORKER_PASSWORD"]),
		MinIOPassword:  string(secret.Data["WORKER_MINIO_PASSWORD"]),
		GatewayKey:     string(secret.Data["WORKER_GATEWAY_KEY"]),
		MatrixToken:    string(secret.Data["WORKER_MATRIX_TOKEN"]),
	}, nil
}

func (s *SecretCredentialStore) Save(ctx context.Context, workerName string, creds *WorkerCredentials) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.secretName(workerName),
			Namespace: s.Namespace,
			Labels: map[string]string{
				"app":                   s.ResourcePrefix.WorkerAppLabel(),
				"hiclaw.io/worker":      workerName,
				v1beta1.LabelController: s.ControllerName,
			},
		},
		Data: map[string][]byte{
			"WORKER_PASSWORD":       []byte(creds.MatrixPassword),
			"WORKER_MINIO_PASSWORD": []byte(creds.MinIOPassword),
			"WORKER_GATEWAY_KEY":    []byte(creds.GatewayKey),
			"WORKER_MATRIX_TOKEN":   []byte(creds.MatrixToken),
		},
	}

	existing, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, s.secretName(workerName), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			_, err = s.Client.CoreV1().Secrets(s.Namespace).Create(ctx, secret, metav1.CreateOptions{})
			return err
		}
		return fmt.Errorf("get credentials secret: %w", err)
	}
	existing.Data = secret.Data
	existing.Labels = secret.Labels
	_, err = s.Client.CoreV1().Secrets(s.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (s *SecretCredentialStore) Delete(ctx context.Context, workerName string) error {
	err := s.Client.CoreV1().Secrets(s.Namespace).Delete(ctx, s.secretName(workerName), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *SecretCredentialStore) List(ctx context.Context) ([]string, error) {
	secrets, err := s.Client.CoreV1().Secrets(s.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: v1beta1.LabelController + "=" + s.ControllerName,
	})
	if err != nil {
		return nil, fmt.Errorf("list credential secrets: %w", err)
	}
	var names []string
	for _, sec := range secrets.Items {
		if name, ok := sec.Labels["hiclaw.io/worker"]; ok && name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}
