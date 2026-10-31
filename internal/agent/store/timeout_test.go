package store

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/pgdial"
)

// TestTimeoutStoreContract runs the shared contract on the bounded memory
// store. The bound must not change any answer that the store gives.
func TestTimeoutStoreContract(t *testing.T) {
	t.Parallel()
	runContract(t, func(t *testing.T, o Options) Store {
		return WithTimeout(NewMemory(o), DefaultCallTimeout)
	})
}

// TestTimeoutStoreBoundsEveryMethod calls each Store method against a server
// that accepts connections and never answers. Each call must end within the
// per-call bound and answer ErrUnavailable.
func TestTimeoutStoreBoundsEveryMethod(t *testing.T) {
	t.Parallel()
	// connect_timeout is raised so the call ends on the per-call deadline,
	// not on the connect timeout.
	dsn := strings.Replace(silentStoreDSN(t), "connect_timeout=1", "connect_timeout=30", 1)
	p, err := Open(context.Background(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	wrapped := WithTimeout(p, 100*time.Millisecond)

	v := reflect.ValueOf(wrapped)
	typ := v.Type()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	if typ.NumMethod() != reflect.TypeOf((*Store)(nil)).Elem().NumMethod() {
		t.Fatalf("method count = %d, want %d", typ.NumMethod(), reflect.TypeOf((*Store)(nil)).Elem().NumMethod())
	}
	for i := range typ.NumMethod() {
		m := typ.Method(i)
		t.Run(m.Name, func(t *testing.T) {
			fn := v.Method(i)
			ft := fn.Type()
			args := make([]reflect.Value, ft.NumIn())
			for j := range ft.NumIn() {
				in := ft.In(j)
				if j == 0 && in == ctxType {
					args[j] = reflect.ValueOf(context.Background())
					continue
				}
				args[j] = reflect.Zero(in)
			}
			start := time.Now()
			var out []reflect.Value
			if ft.IsVariadic() {
				out = fn.CallSlice(args)
			} else {
				out = fn.Call(args)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("%s took %v, want under 1s", m.Name, elapsed)
			}
			last := out[len(out)-1]
			if last.Type() != errType {
				t.Fatalf("%s last result type = %v, want error", m.Name, last.Type())
			}
			err, _ := last.Interface().(error)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%s error = %v, want ErrUnavailable", m.Name, err)
			}
		})
	}
}

// TestTimeoutStoreKeepsNoFailedSchemaCheck asserts that a schema check which
// hit the deadline is not kept. The next call must check again.
func TestTimeoutStoreKeepsNoFailedSchemaCheck(t *testing.T) {
	t.Parallel()
	dsn := strings.Replace(silentStoreDSN(t), "connect_timeout=1", "connect_timeout=30", 1)
	p, err := Open(context.Background(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	_, err = WithTimeout(p, 100*time.Millisecond).GetSettings(context.Background(), "global")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("GetSettings error = %v, want ErrUnavailable", err)
	}
	if p.schemaOK.Load() {
		t.Fatal("schemaOK is true after a call that hit the deadline")
	}
}

// TestTimeoutStoreCallerCancelPassesThrough asserts that a cancel from the
// caller is not reported as an unreachable store.
func TestTimeoutStoreCallerCancelPassesThrough(t *testing.T) {
	t.Parallel()
	dsn := strings.Replace(silentStoreDSN(t), "connect_timeout=1", "connect_timeout=30", 1)
	p, err := Open(context.Background(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = WithTimeout(p, DefaultCallTimeout).GetSettings(ctx, "global")
	if err == nil {
		t.Fatal("GetSettings with a canceled context returned nil")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("caller cancel answered ErrUnavailable: %v", err)
	}
}

// errStore is a Store whose GetSettings returns a fixed error.
type errStore struct {
	Store
	err error
}

func (s errStore) GetSettings(context.Context, string) (agent.Settings, error) {
	return agent.Settings{}, s.err
}

// TestTimeoutStoreClassify asserts the error mapping of a call that did not
// hit the deadline.
func TestTimeoutStoreClassify(t *testing.T) {
	t.Parallel()
	netErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	plain := errors.New("boom")
	tests := []struct {
		name  string
		inner error
		want  error
	}{
		{"nil", nil, nil},
		{"not found passes", ErrNotFound, ErrNotFound},
		{"conflict passes", ErrConflict, ErrConflict},
		{"schema too old passes", ErrSchemaTooOld, ErrSchemaTooOld},
		{"network error is unreachable", netErr, ErrUnavailable},
		{"plain error passes", plain, plain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := WithTimeout(errStore{err: tt.inner}, DefaultCallTimeout)
			_, err := wrapped.GetSettings(context.Background(), "global")
			switch tt.want {
			case nil:
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
			case plain:
				if err != plain {
					t.Fatalf("error = %v, want the inner error", err)
				}
			case ErrUnavailable:
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("error = %v, want ErrUnavailable", err)
				}
			default:
				if !errors.Is(err, tt.want) || errors.Is(err, ErrUnavailable) {
					t.Fatalf("error = %v, want %v and not ErrUnavailable", err, tt.want)
				}
			}
		})
	}
}

// TestDefaultCallTimeoutExceedsConnectTimeout asserts that the per-call bound
// is longer than one connect attempt. The connect timeout must fail first.
func TestDefaultCallTimeoutExceedsConnectTimeout(t *testing.T) {
	t.Parallel()
	if DefaultCallTimeout <= pgdial.DefaultConnectTimeout {
		t.Fatalf("DefaultCallTimeout = %v, want more than %v", DefaultCallTimeout, pgdial.DefaultConnectTimeout)
	}
}
