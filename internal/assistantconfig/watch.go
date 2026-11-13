package assistantconfig

import (
	"context"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
)

var gvrAssistantConfig = v1alpha1.SchemeGroupVersion.WithResource("assistantconfigs")

// Watch follows the AssistantConfig object named v1alpha1.AssistantConfigName
// and stores a new snapshot in live on each change. It returns when ctx ends.
// The informer selects the object by name on the server, and the handlers
// check the name again. Every other object is ignored.
func Watch(ctx context.Context, dyn dynamic.Interface, live *Live, factories map[agent.ProviderKind]provider.Factory,
	keyDir string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll,
		func(o *metav1.ListOptions) {
			o.FieldSelector = "metadata.name=" + v1alpha1.AssistantConfigName
		})
	inf := factory.ForResource(gvrAssistantConfig).Informer()
	apply := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u.GetName() != v1alpha1.AssistantConfigName {
			return
		}
		typed, err := decode(u)
		if err != nil {
			snap := Build(nil, factories, keyDir)
			snap.Err = err
			live.Store(snap)
			logger.Error("assistant config did not decode; the assistant has no providers", "error", err.Error())
			return
		}
		live.Store(Build(typed, factories, keyDir))
	}
	reg, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: apply,
		UpdateFunc: func(_, obj any) {
			apply(obj)
		},
		DeleteFunc: deleteHandler(live, factories, keyDir),
	})
	if err != nil {
		return fmt.Errorf("assistant config handler: %w", err)
	}
	factory.Start(ctx.Done())
	// The registration reports synced only after its handler saw the initial
	// list, so the snapshot is installed when Synced closes.
	if !cache.WaitForCacheSync(ctx.Done(), reg.HasSynced) {
		return fmt.Errorf("assistant config informer did not sync")
	}
	live.markSynced()
	<-ctx.Done()
	factory.Shutdown()
	return nil
}

// decode converts the unstructured object to the typed object.
func decode(u *unstructured.Unstructured) (*v1alpha1.AssistantConfig, error) {
	var out v1alpha1.AssistantConfig
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &out); err != nil {
		return nil, fmt.Errorf("assistant config: %w", err)
	}
	return &out, nil
}

// deleteHandler returns the delete callback of the informer. A watch that was
// interrupted can deliver the last known state wrapped in a tombstone instead
// of the object itself, so the tombstone is unwrapped before the name check.
func deleteHandler(live *Live, factories map[agent.ProviderKind]provider.Factory, keyDir string) func(any) {
	return func(obj any) {
		if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tomb.Obj
		}
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetName() == v1alpha1.AssistantConfigName {
			live.Store(Build(nil, factories, keyDir))
		}
	}
}
