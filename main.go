// SPDX-License-Identifier: Apache-2.0

// kube-pfsense-dns-controller watches Istio VirtualServices and Gateway API HTTPRoutes and
// publishes their hostnames as DNS Resolver host overrides on pfSense, pointing
// at the IP of the shared gateway that serves them.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/controller"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/discover"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

func main() {
	var (
		pfURL       = flag.String("pfsense-url", env("PFSENSE_URL", ""), "pfSense base URL, e.g. https://pfsense.mylab.lan (env PFSENSE_URL)")
		pfCA        = flag.String("pfsense-ca-file", env("PFSENSE_CA_FILE", ""), "CA bundle to trust for the pfSense certificate (env PFSENSE_CA_FILE)")
		pfInsecure  = flag.Bool("pfsense-insecure-skip-verify", envBool("PFSENSE_INSECURE_SKIP_VERIFY", false), "do not verify the pfSense TLS certificate (env PFSENSE_INSECURE_SKIP_VERIFY)")
		domains     = flag.String("domains", env("DNS_DOMAINS", ""), "comma-separated domains to manage, e.g. mylab.lan (env DNS_DOMAINS, required)")
		owner       = flag.String("owner-id", env("OWNER_ID", "k8s-dns-sync"), "ownership tag written to the override description; only tagged entries are changed or deleted")
		prune       = flag.Bool("prune", envBool("PRUNE", true), "delete managed overrides that no longer have a VirtualService/HTTPRoute")
		adopt       = flag.Bool("adopt-existing", envBool("ADOPT_EXISTING", false), "take over unmanaged host overrides whose name matches a route (tags them and fixes their IP) instead of skipping them (env ADOPT_EXISTING)")
		dryRun      = flag.Bool("dry-run", envBool("DRY_RUN", false), "log changes instead of writing to pfSense; with no --pfsense-url pfSense is treated as empty")
		istioNS     = flag.String("istio-gateway-namespaces", env("ISTIO_GATEWAY_NAMESPACES", "istio-system"), "namespaces holding Istio ingress gateway pods/Services")
		enableIstio = flag.Bool("enable-istio", envBool("ENABLE_ISTIO", true), "watch Istio VirtualServices")
		enableGWAPI = flag.Bool("enable-gateway-api", envBool("ENABLE_GATEWAY_API", true), "watch Gateway API HTTPRoutes")
		resync      = flag.Duration("resync-period", envDuration("RESYNC_PERIOD", 2*time.Minute), "full resync interval")
		leaderElect = flag.Bool("leader-elect", envBool("LEADER_ELECT", true), "enable leader election")
		leaseName   = flag.String("leader-election-id", env("LEADER_ELECTION_ID", ""), "name of the leader election Lease; defaults to one derived from --owner-id so separate instances do not share it (env LEADER_ELECTION_ID)")
		pfKeyFile   = flag.String("pfsense-api-key-file", env("PFSENSE_API_KEY_FILE", ""), "file holding the API key, re-read on every request so a rotated Secret is picked up (env PFSENSE_API_KEY_FILE); otherwise PFSENSE_API_KEY is used")
		metricsAddr = flag.String("metrics-bind-address", env("METRICS_ADDR", ":8080"), "metrics endpoint")
		probeAddr   = flag.String("health-probe-bind-address", env("PROBE_ADDR", ":8081"), "health probe endpoint")
	)
	zopts := zap.Options{Development: false}
	zopts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zopts)))
	log := ctrl.Log.WithName("setup")

	doms := records.ParseDomains(*domains)
	if len(doms) == 0 {
		fatal(log, "no domains configured: set --domains or DNS_DOMAINS (for example mylab.lan)")
	}

	// pfSense backend
	var backend pfsense.Backend
	if *pfURL != "" {
		key := pfsense.StaticKey(os.Getenv("PFSENSE_API_KEY"))
		if *pfKeyFile != "" {
			key = pfsense.FileKey(*pfKeyFile)
		}
		c, err := pfsense.New(*pfURL, key, pfsense.Options{
			InsecureSkipVerify: *pfInsecure, CAFile: *pfCA,
		})
		if err != nil {
			fatal(log, "pfsense client: "+err.Error())
		}
		backend = c
	} else if !*dryRun {
		fatal(log, "PFSENSE_URL is required unless --dry-run is set")
	}
	if *dryRun {
		backend = &pfsense.DryRun{Inner: backend, Log: ctrl.Log.WithName("pfsense")}
		log.Info("dry-run enabled: nothing will be written to pfSense")
	}

	scheme := runtime.NewScheme()
	must(log, clientgoscheme.AddToScheme(scheme))
	must(log, gatewayv1.Install(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       leaseID(*leaseName, *owner),
		// Serve Istio (unstructured) reads from the informer cache too.
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: true}},
	})
	if err != nil {
		fatal(log, "manager: "+err.Error())
	}
	must(log, mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(log, mgr.AddReadyzCheck("readyz", healthz.Ping))

	// Use the Istio API version the cluster actually serves.
	vsGVK, igwGVK := discover.VirtualServiceGVK, discover.IstioGatewayGVK
	if *enableIstio {
		var err error
		if vsGVK, err = servedGVK(mgr.GetRESTMapper(), vsGVK); err != nil {
			fatal(log, "Istio VirtualService API not found ("+err.Error()+"); install Istio or set ENABLE_ISTIO=false")
		}
		if igwGVK, err = servedGVK(mgr.GetRESTMapper(), igwGVK); err != nil {
			fatal(log, "Istio Gateway API not found ("+err.Error()+"); install Istio or set ENABLE_ISTIO=false")
		}
		log.Info("using Istio API", "virtualService", vsGVK.Version, "gateway", igwGVK.Version)
	}

	var nss []string
	for _, ns := range strings.Split(*istioNS, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			nss = append(nss, ns)
		}
	}
	r := &controller.Reconciler{
		Discoverer: &discover.Discoverer{
			Client:            mgr.GetClient(),
			APIReader:         mgr.GetAPIReader(),
			Domains:           doms,
			IstioNamespaces:   nss,
			EnableIstio:       *enableIstio,
			EnableGatewayAPI:  *enableGWAPI,
			VirtualServiceGVK: vsGVK,
			IstioGatewayGVK:   igwGVK,
			Log:               ctrl.Log.WithName("discover"),
		},
		Backend: backend,
		Owner:   *owner,
		Prune:   *prune,
		Adopt:   *adopt,
		Log:     ctrl.Log.WithName("sync"),
	}
	must(log, r.Setup(mgr, *resync))

	log.Info("starting", "domains", doms, "owner", *owner, "prune", *prune, "adopt", *adopt, "istio", *enableIstio, "gatewayAPI", *enableGWAPI)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fatal(log, "manager exited: "+err.Error())
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envBool reads a boolean environment variable. An unparseable value is a fatal
// configuration error: silently falling back (e.g. DRY_RUN=yes -> false) could
// turn a dry run into real writes.
func envBool(k string, def bool) bool {
	b, err := parseEnvBool(k, def)
	if err != nil {
		exitConfig(err)
	}
	return b
}

func parseEnvBool(k string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid %s=%q: use true or false", k, v)
	}
	return b, nil
}

// envDuration reads a duration environment variable; unparseable values are fatal.
func envDuration(k string, def time.Duration) time.Duration {
	d, err := parseEnvDuration(k, def)
	if err != nil {
		exitConfig(err)
	}
	return d
}

func parseEnvDuration(k string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid %s=%q: use a positive duration such as 2m or 90s", k, v)
	}
	return d, nil
}

func exitConfig(err error) {
	fmt.Fprintln(os.Stderr, "configuration error:", err)
	os.Exit(2)
}

var nonDNS = regexp.MustCompile(`[^a-z0-9.-]+`)

// leaseID returns the leader election Lease name: the explicit value, or one
// derived from the owner ID so that separate instances (other pfSense targets or
// owners) in one namespace do not compete for a single lease.
func leaseID(explicit, owner string) string {
	if explicit != "" {
		return explicit
	}
	s := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(owner), "-"), "-.")
	if s == "" {
		s = "default"
	}
	id := "kube-pfsense-dns-controller-" + s
	if len(id) > 63 {
		id = strings.TrimRight(id[:63], "-.")
	}
	return id
}

// servedGVK returns gvk with the version the API server prefers for its kind.
func servedGVK(mapper meta.RESTMapper, gvk schema.GroupVersionKind) (schema.GroupVersionKind, error) {
	m, err := mapper.RESTMapping(gvk.GroupKind())
	if err != nil {
		return gvk, err
	}
	return m.GroupVersionKind, nil
}

func must(log logr.Logger, err error) {
	if err != nil {
		log.Error(err, "fatal")
		os.Exit(1)
	}
}

func fatal(log logr.Logger, msg string) {
	log.Error(nil, msg)
	os.Exit(1)
}
