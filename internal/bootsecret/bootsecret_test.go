package bootsecret

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	testNS   = "varroa-system"
	testName = "varroa-ca"
)

var secretGVR = corev1.SchemeGroupVersion.WithResource("secrets")

func testRef() Ref {
	return Ref{Namespace: testNS, Name: testName, Keys: []string{"tls.crt", "tls.key"}}
}

func testOpts() Options {
	return Options{Poll: 10 * time.Millisecond}
}

func ctxWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func storedSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS},
		Data:       data,
	}
}

func countVerb(cs *fake.Clientset, verb string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}

// failGets makes the first n Get calls fail with a transient error.
func failGets(cs *fake.Clientset, n int, msg string) {
	var mu sync.Mutex
	left := n
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if left > 0 {
			left--
			return true, nil, errors.New(msg)
		}
		return false, nil, nil
	})
}

func mintOf(data map[string][]byte, calls *int) MintFunc {
	return func() (map[string][]byte, error) {
		*calls++
		return data, nil
	}
}

func TestLoadOrCreate_LoadsStoredSecret(t *testing.T) {
	stored := map[string][]byte{"tls.crt": []byte("crt"), "tls.key": []byte("key")}
	cs := fake.NewSimpleClientset(storedSecret(stored))
	mints := 0

	got, created, err := LoadOrCreate(ctxWithTimeout(t, time.Second), cs, testRef(), mintOf(nil, &mints), testOpts())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if created {
		t.Fatal("created = true for a stored Secret")
	}
	if string(got["tls.crt"]) != "crt" || string(got["tls.key"]) != "key" {
		t.Fatalf("got stored data %v, want the stored copy", got)
	}
	if mints != 0 {
		t.Fatalf("mint called %d times, want 0", mints)
	}
	for _, verb := range []string{"create", "update", "patch"} {
		if n := countVerb(cs, verb); n != 0 {
			t.Fatalf("%s calls = %d, want 0", verb, n)
		}
	}
}

func TestLoadOrCreate_CreatesOnNotFound(t *testing.T) {
	cs := fake.NewSimpleClientset()
	minted := map[string][]byte{"tls.crt": []byte("new-crt"), "tls.key": []byte("new-key")}
	mints := 0

	got, created, err := LoadOrCreate(ctxWithTimeout(t, time.Second), cs, testRef(), mintOf(minted, &mints), testOpts())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !created {
		t.Fatal("created = false on first install")
	}
	if mints != 1 {
		t.Fatalf("mint called %d times, want 1", mints)
	}
	stored, err := cs.CoreV1().Secrets(testNS).Get(context.Background(), testName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if string(stored.Data["tls.crt"]) != string(got["tls.crt"]) || string(stored.Data["tls.key"]) != string(got["tls.key"]) {
		t.Fatal("stored Secret does not equal the minted data")
	}
}

func TestLoadOrCreate_TransientReadErrorNeverMints(t *testing.T) {
	cs := fake.NewSimpleClientset(storedSecret(map[string][]byte{"tls.crt": []byte("crt"), "tls.key": []byte("key")}))
	failGets(cs, 2, "net/http: TLS handshake timeout")
	mints := 0

	got, created, err := LoadOrCreate(ctxWithTimeout(t, time.Second), cs, testRef(), mintOf(nil, &mints), testOpts())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if created || string(got["tls.crt"]) != "crt" {
		t.Fatalf("created=%v got=%v, want the stored copy", created, got)
	}
	if mints != 0 {
		t.Fatalf("mint called %d times on read errors, want 0", mints)
	}
	if n := countVerb(cs, "create") + countVerb(cs, "update"); n != 0 {
		t.Fatalf("writes = %d, want 0", n)
	}
}

func TestLoadOrCreate_ReadErrorUntilDeadlineNeverMints(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected read failure")
	})
	mints := 0

	_, _, err := LoadOrCreate(ctxWithTimeout(t, 100*time.Millisecond), cs, testRef(), mintOf(nil, &mints), testOpts())
	if err == nil {
		t.Fatal("expected an error after the deadline")
	}
	if !strings.Contains(err.Error(), "injected read failure") {
		t.Fatalf("error %q does not name the read failure", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("a read error must not report ErrNotFound")
	}
	if mints != 0 {
		t.Fatalf("mint called %d times, want 0", mints)
	}
	if n := countVerb(cs, "create"); n != 0 {
		t.Fatalf("create calls = %d, want 0", n)
	}
}

func TestLoadOrCreate_AlreadyExistsUsesStoredSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	competitor := map[string][]byte{"tls.crt": []byte("rival-crt"), "tls.key": []byte("rival-key")}
	var once sync.Once
	cs.PrependReactor("create", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		var handled bool
		once.Do(func() {
			_ = cs.Tracker().Create(secretGVR, storedSecret(competitor), testNS)
			handled = true
		})
		if handled {
			return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), testName)
		}
		return false, nil, nil
	})
	mints := 0

	got, created, err := LoadOrCreate(ctxWithTimeout(t, time.Second), cs, testRef(),
		mintOf(map[string][]byte{"tls.crt": []byte("mine-crt"), "tls.key": []byte("mine-key")}, &mints), testOpts())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if created {
		t.Fatal("created = true when another replica won the race")
	}
	if string(got["tls.crt"]) != "rival-crt" {
		t.Fatalf("got %q, want the competitor's copy", got["tls.crt"])
	}
	stored, _ := cs.CoreV1().Secrets(testNS).Get(context.Background(), testName, metav1.GetOptions{})
	if string(stored.Data["tls.crt"]) != "rival-crt" {
		t.Fatal("stored Secret was replaced")
	}
	if n := countVerb(cs, "update") + countVerb(cs, "patch"); n != 0 {
		t.Fatalf("update/patch calls = %d, want 0", n)
	}
}

func TestLoadOrCreate_CreateErrorRetriesWithSameMint(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var mu sync.Mutex
	failures := 1
	cs.PrependReactor("create", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if failures > 0 {
			failures--
			return true, nil, errors.New("injected create failure")
		}
		return false, nil, nil
	})
	mints := 0

	_, created, err := LoadOrCreate(ctxWithTimeout(t, time.Second), cs, testRef(),
		mintOf(map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k")}, &mints), testOpts())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !created {
		t.Fatal("created = false after the retry succeeded")
	}
	if mints != 1 {
		t.Fatalf("mint called %d times, want 1", mints)
	}
}

func TestLoadOrCreate_LostCreateResponseReadsStored(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var once sync.Once
	cs.PrependReactor("create", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		var handled bool
		once.Do(func() {
			_ = cs.Tracker().Create(secretGVR, action.(clienttesting.CreateAction).GetObject(), testNS)
			handled = true
		})
		if handled {
			return true, nil, errors.New("injected lost response")
		}
		return false, nil, nil
	})
	minted := map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k")}
	mints := 0

	got, _, err := LoadOrCreate(ctxWithTimeout(t, time.Second), cs, testRef(), mintOf(minted, &mints), testOpts())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if string(got["tls.crt"]) != "c" || string(got["tls.key"]) != "k" {
		t.Fatalf("got %v, want the minted data", got)
	}
	if mints != 1 {
		t.Fatalf("mint called %d times, want 1", mints)
	}
	if n := countVerb(cs, "update"); n != 0 {
		t.Fatalf("update calls = %d, want 0", n)
	}
}

func TestLoadOrCreate_IncompleteSecretIsFatal(t *testing.T) {
	cs := fake.NewSimpleClientset(storedSecret(map[string][]byte{"tls.crt": []byte("crt")}))
	mints := 0

	_, _, err := LoadOrCreate(ctxWithTimeout(t, time.Second), cs, testRef(), mintOf(nil, &mints), testOpts())
	var inc *IncompleteError
	if !errors.As(err, &inc) {
		t.Fatalf("error %v, want *IncompleteError", err)
	}
	if inc.Key != "tls.key" {
		t.Fatalf("incomplete key = %q, want tls.key", inc.Key)
	}
	if mints != 0 {
		t.Fatalf("mint called %d times, want 0", mints)
	}
	if n := countVerb(cs, "create") + countVerb(cs, "update") + countVerb(cs, "patch"); n != 0 {
		t.Fatalf("writes = %d, want 0", n)
	}
}

func TestLoadOrCreate_MintErrorIsFatal(t *testing.T) {
	cs := fake.NewSimpleClientset()
	mintErr := errors.New("entropy exhausted")

	_, _, err := LoadOrCreate(ctxWithTimeout(t, time.Second), cs, testRef(),
		func() (map[string][]byte, error) { return nil, mintErr }, testOpts())
	if !errors.Is(err, mintErr) {
		t.Fatalf("error %v, want it to wrap the mint error", err)
	}
	if n := countVerb(cs, "create"); n != 0 {
		t.Fatalf("create calls = %d, want 0", n)
	}
}

func TestLoadOrCreate_RequiresDeadline(t *testing.T) {
	cs := fake.NewSimpleClientset()
	_, _, err := LoadOrCreate(context.Background(), cs, testRef(),
		func() (map[string][]byte, error) { return nil, nil }, testOpts())
	if err == nil {
		t.Fatal("expected an error for a context without a deadline")
	}
}

func TestWait_LoadsWhenPresent(t *testing.T) {
	cs := fake.NewSimpleClientset(storedSecret(map[string][]byte{"tls.crt": []byte("crt"), "tls.key": []byte("key")}))

	got, err := Wait(ctxWithTimeout(t, time.Second), cs, testRef(), testOpts())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(got["tls.key"]) != "key" {
		t.Fatalf("got %v, want the stored data", got)
	}
	assertNoWrites(t, cs)
}

func TestWait_NotFoundUntilDeadline(t *testing.T) {
	cs := fake.NewSimpleClientset()

	_, err := Wait(ctxWithTimeout(t, 100*time.Millisecond), cs, testRef(), testOpts())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error %v, want ErrNotFound", err)
	}
	assertNoWrites(t, cs)
}

func TestWait_ReadErrorUntilDeadlineIsNotNotFound(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected read failure")
	})

	_, err := Wait(ctxWithTimeout(t, 100*time.Millisecond), cs, testRef(), testOpts())
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("error %v, want a non-not-found read error", err)
	}
	assertNoWrites(t, cs)
}

func TestWait_SecretAppearsLate(t *testing.T) {
	cs := fake.NewSimpleClientset()
	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = cs.CoreV1().Secrets(testNS).Create(context.Background(),
			storedSecret(map[string][]byte{"tls.crt": []byte("crt"), "tls.key": []byte("key")}), metav1.CreateOptions{})
	}()

	got, err := Wait(ctxWithTimeout(t, 2*time.Second), cs, testRef(), testOpts())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(got["tls.crt"]) != "crt" {
		t.Fatalf("got %v, want the late Secret", got)
	}
}

func TestWait_IncompleteSecretUntilDeadline(t *testing.T) {
	cs := fake.NewSimpleClientset(storedSecret(map[string][]byte{"tls.key": []byte("key")}))

	_, err := Wait(ctxWithTimeout(t, 100*time.Millisecond), cs, testRef(), testOpts())
	var inc *IncompleteError
	if !errors.As(err, &inc) || inc.Key != "tls.crt" {
		t.Fatalf("error %v, want IncompleteError for tls.crt", err)
	}
	assertNoWrites(t, cs)
}

func assertNoWrites(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	for _, verb := range []string{"create", "update", "patch"} {
		if n := countVerb(cs, verb); n != 0 {
			t.Fatalf("%s calls = %d, want 0", verb, n)
		}
	}
}
