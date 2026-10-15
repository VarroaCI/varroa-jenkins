package runners

import (
	"io"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func metaObj(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Generation: 1}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
