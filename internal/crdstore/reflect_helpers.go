package crdstore

import (
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setDeletionTimestamp sets the DeletionTimestamp pointer on obj's embedded
// ObjectMeta. obj must be a pointer to a struct embedding metav1.ObjectMeta.
func setDeletionTimestamp(obj any, dt *metav1.Time) {
	v := reflect.ValueOf(obj).Elem()
	om := v.FieldByName("ObjectMeta")
	if !om.IsValid() {
		return
	}
	dtField := om.FieldByName("DeletionTimestamp")
	if !dtField.IsValid() || !dtField.CanSet() {
		return
	}
	if dt == nil {
		dtField.Set(reflect.Zero(dtField.Type()))
		return
	}
	t := metav1.NewTime(dt.Time)
	dtField.Set(reflect.ValueOf(&t))
}
