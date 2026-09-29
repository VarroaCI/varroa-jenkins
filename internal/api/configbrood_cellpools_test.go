package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

type recordedRequest struct {
	subject string
	data    []byte
	timeout time.Duration
}

func cellPoolConfigBrood(reply func(subject string, data []byte) ([]byte, error)) (*busConfigBrood, *[]recordedRequest) {
	var calls []recordedRequest
	f := &busConfigBrood{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			calls = append(calls, recordedRequest{subject: subject, data: data, timeout: timeout})
			return reply(subject, data)
		},
	}
	return f, &calls
}

func rawPool(t *testing.T, name string, warm int32) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(&v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: "lts-2-570", Warm: warm},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConfigBrood_ListCellPoolsDecodesItems(t *testing.T) {
	f, calls := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		return json.Marshal(bus.ConfigListResponse{Items: []json.RawMessage{rawPool(t, "member-2-570", 1)}})
	})
	pools, err := f.ListCellPools(context.Background(), "member-c")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pools) != 1 || pools[0].Name != "member-2-570" {
		t.Errorf("pools = %+v", pools)
	}
	if (*calls)[0].subject != "operator.member-c.cellpools.list" || (*calls)[0].timeout != configListTimeout {
		t.Errorf("call = %+v", (*calls)[0])
	}
}

func TestConfigBrood_ScaleCellPoolSendsTheDials(t *testing.T) {
	f, calls := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		return json.Marshal(bus.ConfigGetResponse{Item: rawPool(t, "member-2-570", 3)})
	})
	warm := int32(3)
	pool, err := f.ScaleCellPool(context.Background(), "member-c", bus.CellPoolScaleRequest{Name: "member-2-570", Warm: &warm})
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if pool.Spec.Warm != 3 {
		t.Errorf("warm = %d, want 3", pool.Spec.Warm)
	}
	call := (*calls)[0]
	if call.subject != "operator.member-c.cellpools.scale" || call.timeout != configCellPoolScaleTimeout {
		t.Errorf("call = %s %v", call.subject, call.timeout)
	}
	var sent map[string]any
	_ = json.Unmarshal(call.data, &sent)
	if sent["name"] != "member-2-570" || sent["warm"] != float64(3) || len(sent) != 2 {
		t.Errorf("payload = %v, want the name and warm alone", sent)
	}
}

func TestConfigBrood_CellPoolReplyCodeBecomesABroodError(t *testing.T) {
	f, _ := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		return json.Marshal(bus.ConfigGetResponse{Code: bus.CodeInvalid, Error: "spec.warm must be 0 or above"})
	})
	_, err := f.GetCellPool(context.Background(), "member-c", "member-2-570")
	var be *BroodError
	if !errors.As(err, &be) || be.Code != bus.CodeInvalid || be.Msg != "spec.warm must be 0 or above" {
		t.Errorf("err = %v, want an invalid BroodError", err)
	}
}

func TestConfigBrood_UnreachableNamesTheTargetCluster(t *testing.T) {
	f, _ := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		return nil, errors.New("nats: no responders available for request")
	})
	_, err := f.ListCellPools(context.Background(), "member-c")
	var unreachable *ErrClusterUnreachable
	if !errors.As(err, &unreachable) || unreachable.Cluster != "member-c" {
		t.Errorf("err = %v, want ErrClusterUnreachable naming member-c", err)
	}
}

func TestConfigBrood_CellPoolsRefuseTheLocalCluster(t *testing.T) {
	f, calls := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		t.Fatal("a local-cluster call must send no request")
		return nil, nil
	})
	if _, err := f.ListCellPools(context.Background(), "core"); err == nil {
		t.Error("list on the local cluster must fail")
	}
	if _, err := f.GetCellPool(context.Background(), "core", "p"); err == nil {
		t.Error("get on the local cluster must fail")
	}
	if _, err := f.ScaleCellPool(context.Background(), "core", bus.CellPoolScaleRequest{Name: "p"}); err == nil {
		t.Error("scale on the local cluster must fail")
	}
	if len(*calls) != 0 {
		t.Errorf("calls = %d, want 0", len(*calls))
	}
}
