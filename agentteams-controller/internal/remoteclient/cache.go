package remoteclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credprovider"
)

// maintenanceInterval is the interval between maintenance loop iterations.
const maintenanceInterval = 15 * time.Minute

// renewThreshold is the remaining time before expiration at which
// the maintenance loop considers renewing a cluster's kubeconfig.
const renewThreshold = 2 * time.Hour

// remoteCacheSyncTimeout bounds the time spent waiting for a remote
// cache to sync, so that a slow or unreachable cluster does not block
// reconcile indefinitely.
const remoteCacheSyncTimeout = 30 * time.Second

// ClusterEntry holds a cached remote k8s client and its certificate expiration.
type ClusterEntry struct {
	ClusterID  string
	Client     backend.K8sCoreClient
	RestConfig *rest.Config // used to start a controller-runtime cache against the remote cluster
	Expiration time.Time
	mu         sync.Mutex
	cancel     context.CancelFunc // stops the remote cache for this cluster
}

// CacheConfig holds configuration for creating a Cache.
type CacheConfig struct {
	CredClient     credprovider.Client
	CtrlClient     client.Client // controller-runtime client for querying CRs
	Scheme         *runtime.Scheme
	ControllerName string // label filter applied to remote watches: agentteams.io/controller=<name>
}

// remoteWatch describes a watch registration that should be applied
// against every remote cluster cache that this Cache opens.
type remoteWatch struct {
	ctrl       controller.Controller
	object     client.Object
	handler    handler.EventHandler
	predicates []predicate.Predicate
}

// Cache manages remote k8s clients keyed by cluster ID.
type Cache struct {
	mu             sync.RWMutex
	entries        map[string]*ClusterEntry
	credClient     credprovider.Client
	scheme         *runtime.Scheme
	ctrlClient     client.Client
	controllerName string
	watches        []remoteWatch
	logger         logr.Logger
	// skipHealthCheck disables the /healthz probe in buildEntry. For tests only.
	skipHealthCheck bool
}

// NewCache creates a new remote client cache.
func NewCache(cfg CacheConfig) *Cache {
	return &Cache{
		entries:        make(map[string]*ClusterEntry),
		credClient:     cfg.CredClient,
		scheme:         cfg.Scheme,
		ctrlClient:     cfg.CtrlClient,
		controllerName: cfg.ControllerName,
		logger:         ctrllog.Log.WithName("remoteclient"),
	}
}

// RegisterWatch stores a watch configuration. It must be called at startup
// BEFORE any remote cluster is connected. When a remote cluster cache is
// created, all registered watches are applied via ctrl.Watch(source.Kind(...)).
func (c *Cache) RegisterWatch(ctrl controller.Controller, obj client.Object, h handler.EventHandler, preds ...predicate.Predicate) {
	c.watches = append(c.watches, remoteWatch{ctrl: ctrl, object: obj, handler: h, predicates: preds})
}

// GetOrCreate returns the cached client for the cluster, or creates one
// by calling the STS provider.
func (c *Cache) GetOrCreate(ctx context.Context, clusterID string) (*ClusterEntry, error) {
	// Fast path: read-lock, return if valid.
	c.mu.RLock()
	if entry, ok := c.entries[clusterID]; ok && time.Now().Before(entry.Expiration) {
		c.mu.RUnlock()
		return entry, nil
	}
	c.mu.RUnlock()

	// Slow path: acquire write lock and double-check.
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[clusterID]; ok && time.Now().Before(entry.Expiration) {
		return entry, nil
	}

	c.logger.Info("initializing remote cluster client", "clusterID", clusterID)
	entry, err := c.buildEntry(ctx, clusterID)
	if err != nil {
		c.logger.Error(err, "failed to initialize remote cluster client", "clusterID", clusterID)
		return nil, err
	}
	c.entries[clusterID] = entry
	c.logger.Info("remote cluster client ready", "clusterID", clusterID, "expiration", entry.Expiration, "apiServer", entry.RestConfig.Host)
	return entry, nil
}

// Remove deletes a cached cluster entry and stops its informer.
func (c *Cache) Remove(clusterID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[clusterID]; ok {
		c.logger.Info("removing remote cluster client", "clusterID", clusterID)
		if entry.cancel != nil {
			entry.cancel()
		}
		delete(c.entries, clusterID)
	}
}

// StartMaintenanceLoop starts a background goroutine that checks every 15 minutes:
//   - For each entry where expiration − now > 2h: skip.
//   - For each entry where expiration − now ≤ 2h:
//     1. Query if any object still references this cluster.
//     2. If no workers deployed: Remove the entry.
//     3. If workers deployed: call STS to renew kubeconfig, rebuild client, update expiration.
func (c *Cache) StartMaintenanceLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(maintenanceInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.maintain(ctx)
			}
		}
	}()
}

// maintain runs one maintenance pass over all cached entries.
func (c *Cache) maintain(ctx context.Context) {
	// Snapshot current cluster IDs and expirations under read lock.
	type snapshot struct {
		clusterID  string
		expiration time.Time
	}
	c.mu.RLock()
	entries := make([]snapshot, 0, len(c.entries))
	for id, e := range c.entries {
		entries = append(entries, snapshot{clusterID: id, expiration: e.Expiration})
	}
	c.mu.RUnlock()

	now := time.Now()
	for _, s := range entries {
		remaining := s.expiration.Sub(now)
		if remaining > renewThreshold {
			continue
		}

		deployed, err := c.hasWorkersDeployed(ctx, s.clusterID)
		if err != nil {
			c.logger.Error(err, "failed to check workers for cluster", "clusterID", s.clusterID)
			continue
		}
		if !deployed {
			c.logger.Info("no workers deployed, removing cached client", "clusterID", s.clusterID)
			c.Remove(s.clusterID)
			continue
		}

		// Renew kubeconfig for clusters that still have deployed workers.
		if err := c.renewEntry(ctx, s.clusterID); err != nil {
			c.logger.Error(err, "failed to renew kubeconfig", "clusterID", s.clusterID)
			// Will be retried on next cycle.
		}
	}
}

// renewEntry rebuilds the k8s client for the given cluster using a fresh
// kubeconfig from the STS provider.
func (c *Cache) renewEntry(ctx context.Context, clusterID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[clusterID]
	if !ok {
		return nil // removed between snapshot and renewal
	}

	c.logger.Info("renewing remote cluster credentials", "clusterID", clusterID)
	newEntry, err := c.buildEntry(ctx, clusterID)
	if err != nil {
		return err
	}

	// Stop old informer before replacing.
	if entry.cancel != nil {
		entry.cancel()
	}

	c.entries[clusterID] = newEntry
	c.logger.Info("remote cluster credentials renewed", "clusterID", clusterID, "newExpiration", newEntry.Expiration)
	return nil
}

// buildEntry fetches a kubeconfig from the STS provider and constructs a
// ClusterEntry. Caller must hold c.mu (write).
func (c *Cache) buildEntry(ctx context.Context, clusterID string) (*ClusterEntry, error) {
	resp, err := c.credClient.GetKubeconfig(ctx, clusterID)
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig for cluster %s: %w", clusterID, err)
	}

	restCfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(resp.Kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig for cluster %s: %w", clusterID, err)
	}

	coreClient, err := corev1client.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create core client for cluster %s: %w", clusterID, err)
	}

	authClient, err := authenticationv1client.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create auth client for cluster %s: %w", clusterID, err)
	}

	// Health check: verify remote cluster is reachable before caching.
	if !c.skipHealthCheck {
		healthCtx, healthCancel := context.WithTimeout(ctx, 10*time.Second)
		defer healthCancel()
		if _, err := coreClient.RESTClient().Get().AbsPath("/healthz").DoRaw(healthCtx); err != nil {
			return nil, fmt.Errorf("health check failed for cluster %s: %w", clusterID, err)
		}
	}

	expiration, err := time.Parse(time.RFC3339, resp.Expiration)
	if err != nil {
		return nil, fmt.Errorf("parse expiration for cluster %s: %w", clusterID, err)
	}

	// Derive a per-entry context so the remote cache stops when the entry
	// is removed or replaced (Remove / renewEntry call entry.cancel).
	entryCtx, cancel := context.WithCancel(context.Background())
	entry := &ClusterEntry{
		ClusterID:  clusterID,
		Client:     &k8sCoreClientWrapper{client: coreClient, authClient: authClient},
		RestConfig: restCfg,
		Expiration: expiration,
		cancel:     cancel,
	}

	// Start the remote cache and apply registered watches. Failures here
	// are non-fatal: remote watches are an enhancement on top of the
	// basic client cache, and reconcile will still function via polling.
	if err := c.startRemoteCache(entryCtx, restCfg, clusterID); err != nil {
		c.logger.Error(err, "failed to start remote cache", "clusterID", clusterID)
	}

	return entry, nil
}

// startRemoteCache starts a controller-runtime cache against the remote
// cluster and applies every registered watch. The cache lifecycle is bound
// to ctx: when ctx is cancelled (via entry.cancel), the cache stops.
func (c *Cache) startRemoteCache(ctx context.Context, restCfg *rest.Config, clusterID string) error {
	if len(c.watches) == 0 {
		return nil
	}

	c.logger.Info("starting remote cache", "clusterID", clusterID, "watches", len(c.watches))

	labelSelector := labels.SelectorFromSet(labels.Set{
		"agentteams.io/controller": c.controllerName,
	})
	byObject := make(map[client.Object]ctrlcache.ByObject, len(c.watches))
	for _, w := range c.watches {
		byObject[w.object] = ctrlcache.ByObject{Label: labelSelector}
	}

	remoteCache, err := ctrlcache.New(restCfg, ctrlcache.Options{ByObject: byObject})
	if err != nil {
		return fmt.Errorf("create remote cache: %w", err)
	}

	go func() {
		if err := remoteCache.Start(ctx); err != nil && ctx.Err() == nil {
			c.logger.Error(err, "remote cache stopped unexpectedly", "clusterID", clusterID)
		}
	}()

	// Bound the wait so a slow or unreachable remote cluster does not
	// block reconcile for an unbounded amount of time.
	syncCtx, cancel := context.WithTimeout(ctx, remoteCacheSyncTimeout)
	defer cancel()
	if !remoteCache.WaitForCacheSync(syncCtx) {
		return fmt.Errorf("remote cache sync failed or timed out after %s", remoteCacheSyncTimeout)
	}
	c.logger.Info("remote cache synced", "clusterID", clusterID)

	for _, w := range c.watches {
		src := source.Kind(remoteCache, w.object, w.handler, w.predicates...)
		if err := w.ctrl.Watch(src); err != nil {
			return fmt.Errorf("register watch for %T: %w", w.object, err)
		}
	}
	return nil
}

// hasWorkersDeployed checks whether any Worker CR references the given cluster.
// The open-source Worker API no longer exposes remote target clusters, so this
// currently always returns false.
func (c *Cache) hasWorkersDeployed(ctx context.Context, clusterID string) (bool, error) {
	_ = ctx
	_ = clusterID
	return false, nil
}

// ResolveClient returns the cached K8sCoreClient for the given cluster ID.
// This implements backend.RemoteClientProvider.
func (c *Cache) ResolveClient(ctx context.Context, clusterID string) (backend.K8sCoreClient, error) {
	entry, err := c.GetOrCreate(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return entry.Client, nil
}

// ResolveDynamicClient returns a dynamic client for the given remote cluster.
// Sandbox backends need this for provider CRDs such as SandboxClaim.
func (c *Cache) ResolveDynamicClient(ctx context.Context, clusterID string) (dynamic.Interface, error) {
	entry, err := c.GetOrCreate(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(entry.RestConfig)
}

// k8sCoreClientWrapper adapts *corev1client.CoreV1Client to backend.K8sCoreClient.
type k8sCoreClientWrapper struct {
	client     *corev1client.CoreV1Client
	authClient *authenticationv1client.AuthenticationV1Client
}

func (w *k8sCoreClientWrapper) Pods(namespace string) backend.K8sPodClient {
	return w.client.Pods(namespace)
}

func (w *k8sCoreClientWrapper) ConfigMaps(namespace string) backend.K8sConfigMapClient {
	return w.client.ConfigMaps(namespace)
}

func (w *k8sCoreClientWrapper) Services(namespace string) backend.K8sServiceClient {
	return w.client.Services(namespace)
}

func (w *k8sCoreClientWrapper) Namespaces() backend.K8sNamespaceClient {
	return w.client.Namespaces()
}

func (w *k8sCoreClientWrapper) ServiceAccounts(namespace string) backend.K8sServiceAccountClient {
	return w.client.ServiceAccounts(namespace)
}

func (w *k8sCoreClientWrapper) TokenReviews() backend.K8sTokenReviewClient {
	return w.authClient.TokenReviews()
}
