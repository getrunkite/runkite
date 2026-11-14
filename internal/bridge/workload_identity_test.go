package bridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type workloadTestPKI struct {
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newWorkloadTestPKI(t *testing.T) *workloadTestPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test external CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &workloadTestPKI{ca: ca, key: key, pool: pool}
}

func (p *workloadTestPKI) certificate(t *testing.T, uri string, server bool, ttl time.Duration) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "not an authority"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(ttl), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = []*url.URL{u}
	}
	if server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	raw, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key, Leaf: leaf}
}

func workloadTestTokens(t *testing.T) *auth.RunnerTokens {
	t.Helper()
	t.Setenv("RUNNER_TOKEN_WORKLOAD_TEST", "synthetic-workload-token")
	rt := auth.LoadRunnerTokensFromEnv()
	if err := rt.ConfigureWorkloadIdentities(map[string][]string{"workload-test": {"spiffe://test.runkite/runner/fleet"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	return rt
}

func workloadClient(t *testing.T, address string, pki *workloadTestPKI, certificate tls.Certificate) pb.RunnerServiceClient {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pki.pool, Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return pb.NewRunnerServiceClient(connection)
}

func workloadCallContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("runner-kind", "workload-test", "runner-token", "synthetic-workload-token"))
}

func TestWorkloadIdentityMTLSSameFleetPeerCannotHijack(t *testing.T) {
	pki := newWorkloadTestPKI(t)
	serverCert := pki.certificate(t, "", true, time.Hour)
	workerA := pki.certificate(t, "spiffe://test.runkite/runner/fleet", false, 30*time.Minute)
	workerB := pki.certificate(t, "spiffe://test.runkite/runner/fleet", false, 30*time.Minute)
	unauthorized := pki.certificate(t, "spiffe://test.runkite/runner/other", false, 30*time.Minute)
	rt := workloadTestTokens(t)
	queue, broker, cancels := inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()
	var completions atomic.Int32
	bridge := NewServer(queue, broker, cancels, func(_, _, _ string) { completions.Add(1) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pki.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})), grpc.UnaryInterceptor(UnaryAuthInterceptor(rt)), grpc.StreamInterceptor(StreamAuthInterceptor(rt)))
	pb.RegisterRunnerServiceServer(server, bridge)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	a := workloadClient(t, listener.Addr().String(), pki, workerA)
	b := workloadClient(t, listener.Addr().String(), pki, workerB)
	bad := workloadClient(t, listener.Addr().String(), pki, unauthorized)
	if _, err := bad.GetJob(workloadCallContext(t), &pb.GetJobRequest{RunnerKind: "workload-test", TimeoutSeconds: 1}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unmapped verified SAN: %v", err)
	}
	if err := queue.Enqueue(context.Background(), &transport.RunAssignment{RunID: "owned-run", RunnerKind: "workload-test", ThreadID: "thread", GraphID: "agent", TenantID: "acme", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	job, err := a.GetJob(workloadCallContext(t), &pb.GetJobRequest{RunnerKind: "workload-test", TimeoutSeconds: 1})
	if err != nil || !job.HasJob {
		t.Fatalf("dispatch: %v, %v", job, err)
	}
	var assignment transport.RunAssignment
	if err := json.Unmarshal([]byte(job.AssignmentJson), &assignment); err != nil {
		t.Fatal(err)
	}
	if assignment.WorkloadOwner == nil || assignment.WorkloadOwner.URI != "spiffe://test.runkite/runner/fleet" || len(assignment.WorkloadOwner.KeySHA256) != 64 {
		t.Fatalf("missing verified owner: %#v", assignment.WorkloadOwner)
	}
	if _, err := b.Heartbeat(workloadCallContext(t), &pb.HeartbeatRequest{RunId: assignment.RunID, Generation: 1}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("peer renewed owner lease: %v", err)
	}
	stream, err := b.StreamEvents(workloadCallContext(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&pb.RunEventProto{RunId: assignment.RunID, Generation: 1, EventJson: `{"event_id":"peer","method":"values","data":{"secret":"must not publish"}}`})
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("peer published: %v", err)
	}
	events, err := broker.Replay(context.Background(), assignment.RunID, 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("peer event was stored: %v, %v", events, err)
	}
	if _, err := b.ReportStatus(workloadCallContext(t), &pb.ReportStatusRequest{RunId: assignment.RunID, Generation: 1, Status: "success"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("peer completed: %v", err)
	}
	if completions.Load() != 0 {
		t.Fatal("peer changed durable result")
	}
	var httpEffects atomic.Int32
	handler := auth.MiddlewareWithOpts(nil, nil, rt, auth.MiddlewareOpts{Inflight: queue}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		binding := auth.RunBindingFromContext(r.Context())
		if binding == nil || binding.WorkloadIdentity == nil || binding.TenantID != "acme" {
			t.Error("HTTP lost verified assignment proof")
		}
		httpEffects.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	httpServer := httptest.NewUnstartedServer(handler)
	httpServer.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pki.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)
	request := func(cert tls.Certificate, path string) int {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pki.pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}, Timeout: 2 * time.Second}
		defer client.CloseIdleConnections()
		req, err := http.NewRequest("GET", httpServer.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(auth.HeaderRunnerKind, "workload-test")
		req.Header.Set(auth.HeaderRunnerToken, "synthetic-workload-token")
		req.Header.Set(auth.HeaderRunID, assignment.RunID)
		req.Header.Set(auth.HeaderGeneration, strconv.FormatInt(assignment.Generation, 10))
		req.Header.Set("X-Workload-Identity", assignment.WorkloadOwner.URI)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := request(workerB, "/internal/store/items"); got != http.StatusForbidden {
		t.Fatalf("peer HTTP status=%d", got)
	}
	if got := request(workerA, "/internal/runs/foreign/status"); got != http.StatusForbidden {
		t.Fatalf("foreign run path status=%d", got)
	}
	if got := request(workerA, "/internal/store/items"); got != http.StatusNoContent {
		t.Fatalf("owner HTTP status=%d", got)
	}
	if httpEffects.Load() != 1 {
		t.Fatalf("unexpected HTTP effects=%d", httpEffects.Load())
	}
	// Reclaim captures a different key only at a new generation.
	if n, _, err := queue.ReclaimStale(context.Background(), -time.Second, 0); err != nil || n != 1 {
		t.Fatalf("reclaim: %d %v", n, err)
	}
	replacement, err := b.GetJob(workloadCallContext(t), &pb.GetJobRequest{RunnerKind: "workload-test", TimeoutSeconds: 1})
	if err != nil || !replacement.HasJob {
		t.Fatalf("replacement: %v %v", replacement, err)
	}
	if _, err := a.Heartbeat(workloadCallContext(t), &pb.HeartbeatRequest{RunId: assignment.RunID, Generation: 2}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("old key borrowed replacement generation: %v", err)
	}
	if resp, err := b.Heartbeat(workloadCallContext(t), &pb.HeartbeatRequest{RunId: assignment.RunID, Generation: 2}); err != nil || resp.Superseded {
		t.Fatalf("new owner: %v %v", resp, err)
	}
}

func TestWorkloadIdentityRejectsUnverifiedExpiredAndLongLivedCertificates(t *testing.T) {
	pki := newWorkloadTestPKI(t)
	rt := workloadTestTokens(t)
	for _, test := range []struct {
		name, uri string
		ttl       time.Duration
		verified  bool
	}{
		{"unverified", "spiffe://test.runkite/runner/fleet", 30 * time.Minute, false},
		{"expired", "spiffe://test.runkite/runner/fleet", -time.Second, true},
		{"long_lived", "spiffe://test.runkite/runner/fleet", 2 * time.Hour, true},
		{"CN_is_not_SAN", "", 30 * time.Minute, true},
		{"unmapped", "spiffe://test.runkite/runner/foreign", 30 * time.Minute, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cert := pki.certificate(t, test.uri, false, test.ttl)
			state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}
			if test.verified {
				state.VerifiedChains = [][]*x509.Certificate{{cert.Leaf, pki.ca}}
			}
			if _, err := rt.AuthenticateWorkloadTLS(context.Background(), state, "workload-test"); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
	owner := &transport.WorkloadIdentity{URI: "spiffe://test.runkite/runner/fleet", KeySHA256: strings.Repeat("a", 64), NotAfter: time.Now().Add(-time.Second)}
	ctx := transport.WithWorkloadIdentity(context.Background(), owner)
	if err := auth.RequireWorkloadOwner(ctx, owner); err == nil {
		t.Fatal("expired existing connection remained authorized")
	}
	if _, err := NewServer(inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus(), nil).Heartbeat(ctx, &pb.HeartbeatRequest{RunId: "run"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expired RPC: %v", err)
	}
}

func TestWorkloadCancelWatchersAreHolderScoped(t *testing.T) {
	queue, cancels := inprocess.NewQueue(), inprocess.NewCancelBus()
	bridge := NewServer(queue, inprocess.NewBroker(), cancels, nil)
	ownerA := &transport.WorkloadIdentity{URI: "spiffe://test.runkite/runner/fleet", KeySHA256: strings.Repeat("a", 64), NotAfter: time.Now().Add(time.Hour)}
	ownerB := *ownerA
	ownerB.KeySHA256 = strings.Repeat("b", 64)
	ctxA, ctxB := transport.WithWorkloadIdentity(context.Background(), ownerA), transport.WithWorkloadIdentity(context.Background(), &ownerB)
	a, b := make(chan string, 1), make(chan string, 1)
	bridge.addWatcher(workloadWatcherKey(ctxA, "kind"), a)
	bridge.addWatcher(workloadWatcherKey(ctxB, "kind"), b)
	defer bridge.removeWatcher(workloadWatcherKey(ctxA, "kind"), a)
	defer bridge.removeWatcher(workloadWatcherKey(ctxB, "kind"), b)
	if err := queue.Enqueue(context.Background(), &transport.RunAssignment{RunID: "cancel-owned", RunnerKind: "kind", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if job, err := bridge.GetJob(ctxA, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1}); err != nil || !job.HasJob {
		t.Fatalf("dispatch: %v %v", job, err)
	}
	defer bridge.cleanupRun("cancel-owned")
	if err := cancels.PublishCancel(context.Background(), "cancel-owned"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-a:
		if id != "cancel-owned" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("owner missed cancel")
	}
	select {
	case id := <-b:
		t.Fatalf("peer received another holder's cancellation %q", id)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestWorkloadIdentityRetiredKeyDoesNotRetireItsServicePeers(t *testing.T) {
	pki := newWorkloadTestPKI(t)
	a := pki.certificate(t, "spiffe://test.runkite/runner/fleet", false, 30*time.Minute)
	b := pki.certificate(t, "spiffe://test.runkite/runner/fleet", false, 30*time.Minute)
	digest := sha256.Sum256(a.Leaf.RawSubjectPublicKeyInfo)
	rt := workloadTestTokens(t)
	if err := rt.ConfigureWorkloadIdentityPolicy(map[string][]string{"workload-test": {"spiffe://test.runkite/runner/fleet"}}, []string{hex.EncodeToString(digest[:])}, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		cert   tls.Certificate
		denied bool
	}{{a, true}, {b, false}} {
		state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{test.cert.Leaf}, VerifiedChains: [][]*x509.Certificate{{test.cert.Leaf, pki.ca}}}
		ctx, err := rt.AuthenticateWorkloadTLS(context.Background(), state, "workload-test")
		if (err != nil) != test.denied {
			t.Fatalf("retired=%v err=%v", test.denied, err)
		}
		if !test.denied {
			if err := auth.RequireWorkloadAuthority(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
}
