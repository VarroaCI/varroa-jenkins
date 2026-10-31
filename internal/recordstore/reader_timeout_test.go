package recordstore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/pgdial"
)

// unreachableListener accepts TCP connections and never writes. A Postgres
// client that waits for the server reply waits until its own timeout.
func unreachableListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return ln.Addr().String()
}

// unreachableReader returns a Reader whose pool never gets a connection. The
// connect timeout is long, so only the per-call deadline ends each call.
func unreachableReader(t *testing.T) Reader {
	t.Helper()
	addr := unreachableListener(t)
	pool, err := pgdial.NewPool(context.Background(), "postgres://u:p@"+addr+"/db?sslmode=disable&connect_timeout=30")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewReader(pool)
}

func TestSchemaVersionUnreachableIsErrUnavailable(t *testing.T) {
	addr := unreachableListener(t)
	pool, err := pgdial.NewPool(context.Background(), "postgres://u:p@"+addr+"/db?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, err = NewReader(pool).SchemaVersion(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("SchemaVersion error = %v, want ErrUnavailable", err)
	}
	if errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("SchemaVersion error = %v, must not be ErrSchemaTooOld", err)
	}
}

// TestCheckSchemaDoesNotKeepAFailure asserts a failed schema check leaves the
// Reader unchecked, so the next call runs the check again.
func TestCheckSchemaDoesNotKeepAFailure(t *testing.T) {
	t.Parallel()

	r := unreachableReader(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := r.ListJobSummaries(ctx, testCluster, testTenant, time.Now().Add(-30*24*time.Hour)); err == nil {
		t.Fatal("ListJobSummaries against a silent server succeeded")
	}
	if r.(*pgReader).schemaOK.Load() {
		t.Fatal("schemaOK is true after a failed schema check")
	}
}

// zeroArgs builds the arguments of one Reader method call: a background
// context, and the zero value of every other parameter. A method that
// validates an argument before it reaches the database gets a valid value
// in its place, so the call still reaches the unreachable store.
func zeroArgs(name string, fn reflect.Value) []reflect.Value {
	args := make([]reflect.Value, 0, fn.Type().NumIn())
	args = append(args, reflect.ValueOf(context.Background()))
	for i := 1; i < fn.Type().NumIn(); i++ {
		if name == "RevisionJobs" && fn.Type().In(i).Kind() == reflect.String && i == 3 {
			args = append(args, reflect.ValueOf(strings.Repeat("0", 40)))
			continue
		}
		args = append(args, reflect.Zero(fn.Type().In(i)))
	}
	return args
}

func TestTimeoutReaderBoundsEveryMethod(t *testing.T) {
	inner := unreachableReader(t)
	wrapped := WithTimeout(inner, 100*time.Millisecond)
	readerType := reflect.TypeOf((*Reader)(nil)).Elem()
	value := reflect.ValueOf(wrapped)
	if readerType.NumMethod() == 0 {
		t.Fatal("Reader has no methods")
	}
	for i := 0; i < readerType.NumMethod(); i++ {
		m := readerType.Method(i)
		t.Run(m.Name, func(t *testing.T) {
			fn := value.MethodByName(m.Name)
			start := time.Now()
			out := callGuarded(t, m.Name, fn, zeroArgs(m.Name, fn))
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("%s took %v, want under 1s", m.Name, elapsed)
			}
			errVal := out[len(out)-1]
			if errVal.IsNil() {
				return
			}
			err, ok := errVal.Interface().(error)
			if !ok {
				t.Fatalf("%s last result is %T, want error", m.Name, errVal.Interface())
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%s error = %v, want ErrUnavailable", m.Name, err)
			}
		})
	}
}

// callGuarded calls fn and fails the test on a panic. A zero argument can
// reach a nil dependency, so the panic message names the method for triage.
func callGuarded(t *testing.T, name string, fn reflect.Value, args []reflect.Value) (out []reflect.Value) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked with zero arguments: %v", name, fmt.Sprint(r))
		}
	}()
	return fn.Call(args)
}

// fakeGetRunReader serves GetRun from a fixed error. Other methods are unused.
type fakeGetRunReader struct {
	Reader
	block bool
	err   error
}

func (f fakeGetRunReader) GetRun(ctx context.Context, runID string) (Run, []Stage, error) {
	if f.block {
		<-ctx.Done()
		return Run{}, nil, ctx.Err()
	}
	return Run{}, nil, f.err
}

func TestTimeoutReaderPassesThroughOtherErrors(t *testing.T) {
	r := WithTimeout(fakeGetRunReader{err: ErrRunNotFound}, time.Second)
	_, _, err := r.GetRun(context.Background(), "x")
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("GetRun error = %v, want ErrRunNotFound", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("GetRun error = %v, must not be ErrUnavailable", err)
	}
}

func TestTimeoutReaderKeepsCallerCancel(t *testing.T) {
	r := WithTimeout(fakeGetRunReader{block: true}, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, _, err := r.GetRun(ctx, "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GetRun error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("a caller cancel must not become ErrUnavailable: %v", err)
	}
}
