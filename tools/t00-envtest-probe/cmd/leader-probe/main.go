package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	namespace = "sentry-operator-leader-probe"
	leaseName = "leader-probe"
)

func main() {
	contextName := flag.String("context", "kind-sentry-operator-dev", "kubeconfig context")
	flag.Parse()

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: *contextName}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, overrides,
	).ClientConfig()
	check(err, "load kubeconfig")

	client, err := kubernetes.NewForConfig(config)
	check(err, "create Kubernetes client")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		check(err, "create probe namespace")
	}
	defer func() {
		deleteContext, deleteCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer deleteCancel()
		_ = client.CoreV1().Namespaces().Delete(
			deleteContext, namespace, metav1.DeleteOptions{},
		)
	}()
	_ = client.CoordinationV1().Leases(namespace).Delete(
		ctx, leaseName, metav1.DeleteOptions{},
	)

	leaders := make(chan string, 4)
	cancels := map[string]context.CancelFunc{}
	for _, identity := range []string{"probe-a", "probe-b"} {
		candidateContext, candidateCancel := context.WithCancel(ctx)
		cancels[identity] = candidateCancel
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{Name: leaseName, Namespace: namespace},
			Client:    client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: identity,
			},
		}
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   6 * time.Second,
			RenewDeadline:   4 * time.Second,
			RetryPeriod:     time.Second,
			ReleaseOnCancel: true,
			Name:            "t00-leader-probe",
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(_ context.Context) {
					leaders <- identity
				},
				OnStoppedLeading: func() {},
			},
		})
		check(err, "create leader elector")
		go elector.Run(candidateContext)
	}

	first := receiveLeader(ctx, leaders, "initial leader")
	cancels[first]()

	var second string
	for second == "" {
		candidate := receiveLeader(ctx, leaders, "failover leader")
		if candidate != first {
			second = candidate
		}
	}

	lease, err := client.CoordinationV1().Leases(namespace).Get(
		ctx, leaseName, metav1.GetOptions{},
	)
	check(err, "read leader lease")
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != second {
		check(fmt.Errorf("expected holder %q, got %#v", second, lease.Spec.HolderIdentity), "verify failover")
	}
	cancels[second]()

	fmt.Printf("%s: leader election failover %s -> %s passed\n", *contextName, first, second)
}

func receiveLeader(ctx context.Context, leaders <-chan string, phase string) string {
	select {
	case identity := <-leaders:
		return identity
	case <-ctx.Done():
		check(ctx.Err(), phase)
		return ""
	}
}

func check(err error, action string) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
	os.Exit(1)
}
