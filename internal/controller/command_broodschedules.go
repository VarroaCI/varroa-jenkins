package controller

import (
	"context"
	"encoding/json"
	"log/slog"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// CommandBroodSchedules implements operator-side brood-schedule command handlers
// for BFF-initiated commands received over the NATS bus.
type CommandBroodSchedules struct {
	client.Client
	OperatorNamespace string
	Logger            *slog.Logger
}

// NewCommandBroodSchedules creates a new CommandBroodSchedules.
func NewCommandBroodSchedules(c client.Client, operatorNS string, logger *slog.Logger) *CommandBroodSchedules {
	return &CommandBroodSchedules{
		Client:            c,
		OperatorNamespace: operatorNS,
		Logger:            logger.With("component", "command_broodschedules"),
	}
}

// reply marshals v to JSON for the bus response.
func (c *CommandBroodSchedules) reply(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		c.Logger.Error("broodschedules reply marshal failed", "error", err)
		errResp, _ := json.Marshal(map[string]string{"error": "internal marshal error"})
		return errResp
	}
	return data
}

// HandleCreate handles operator.<cluster>.broodschedules.create.
func (c *CommandBroodSchedules) HandleCreate(data []byte) []byte {
	var req bus.BroodScheduleCreateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.reply(bus.BroodScheduleResponse{Error: "invalid request"})
	}

	ctx := context.Background()

	var spec v1alpha1.BroodScheduleSpec
	if err := json.Unmarshal(req.Spec, &spec); err != nil {
		return c.reply(bus.BroodScheduleResponse{Error: "invalid spec"})
	}

	sched := &v1alpha1.BroodSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
		Spec: spec,
	}

	if err := c.Create(ctx, sched); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return c.reply(bus.BroodScheduleResponse{Error: "already exists"})
		}
		c.Logger.Error("broodschedules create failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.reply(bus.BroodScheduleResponse{Error: err.Error()})
	}

	specRaw, _ := json.Marshal(sched.Spec)
	statusRaw, _ := json.Marshal(sched.Status)
	return c.reply(bus.BroodScheduleResponse{
		Namespace: sched.Namespace,
		Name:      sched.Name,
		Spec:      specRaw,
		Status:    statusRaw,
	})
}

// HandleGet handles operator.<cluster>.broodschedules.get.
func (c *CommandBroodSchedules) HandleGet(data []byte) []byte {
	var req bus.BroodScheduleGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.reply(bus.BroodScheduleResponse{Error: "invalid request"})
	}

	ctx := context.Background()

	var sched v1alpha1.BroodSchedule
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &sched); err != nil {
		if k8serrors.IsNotFound(err) {
			return c.reply(bus.BroodScheduleResponse{Error: "not found"})
		}
		c.Logger.Error("broodschedules get failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.reply(bus.BroodScheduleResponse{Error: err.Error()})
	}

	specRaw, _ := json.Marshal(sched.Spec)
	statusRaw, _ := json.Marshal(sched.Status)
	return c.reply(bus.BroodScheduleResponse{
		Namespace: sched.Namespace,
		Name:      sched.Name,
		Spec:      specRaw,
		Status:    statusRaw,
	})
}

// HandleList handles operator.<cluster>.broodschedules.list.
func (c *CommandBroodSchedules) HandleList(data []byte) []byte {
	var req bus.BroodScheduleListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.reply(bus.BroodScheduleListResponse{Error: "invalid request"})
	}

	ctx := context.Background()

	var list v1alpha1.BroodScheduleList
	var listOpts []client.ListOption
	if req.Namespace != "" {
		listOpts = append(listOpts, client.InNamespace(req.Namespace))
	}
	if err := c.List(ctx, &list, listOpts...); err != nil {
		c.Logger.Error("broodschedules list failed", "namespace", req.Namespace, "error", err)
		return c.reply(bus.BroodScheduleListResponse{Error: err.Error()})
	}

	items := make([]bus.BroodScheduleResponse, 0, len(list.Items))
	for i := range list.Items {
		specRaw, _ := json.Marshal(list.Items[i].Spec)
		statusRaw, _ := json.Marshal(list.Items[i].Status)
		items = append(items, bus.BroodScheduleResponse{
			Namespace: list.Items[i].Namespace,
			Name:      list.Items[i].Name,
			Spec:      specRaw,
			Status:    statusRaw,
		})
	}

	return c.reply(bus.BroodScheduleListResponse{Items: items})
}

// HandleDelete handles operator.<cluster>.broodschedules.delete.
func (c *CommandBroodSchedules) HandleDelete(data []byte) []byte {
	var req bus.BroodScheduleDeleteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.reply(bus.BroodScheduleResponse{Error: "invalid request"})
	}

	ctx := context.Background()

	var sched v1alpha1.BroodSchedule
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &sched); err != nil {
		if k8serrors.IsNotFound(err) {
			return c.reply(bus.BroodScheduleResponse{Error: "not found"})
		}
		c.Logger.Error("broodschedules delete get failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.reply(bus.BroodScheduleResponse{Error: err.Error()})
	}

	if err := c.Delete(ctx, &sched); err != nil {
		c.Logger.Error("broodschedules delete failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.reply(bus.BroodScheduleResponse{Error: err.Error()})
	}

	return c.reply(bus.BroodScheduleResponse{})
}

// HandleSuspend handles operator.<cluster>.broodschedules.suspend.
func (c *CommandBroodSchedules) HandleSuspend(data []byte) []byte {
	var req bus.BroodScheduleSuspendRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.reply(bus.BroodScheduleResponse{Error: "invalid request"})
	}

	ctx := context.Background()

	var sched v1alpha1.BroodSchedule
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &sched); err != nil {
		if k8serrors.IsNotFound(err) {
			return c.reply(bus.BroodScheduleResponse{Error: "not found"})
		}
		c.Logger.Error("broodschedules suspend get failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.reply(bus.BroodScheduleResponse{Error: err.Error()})
	}

	sched.Spec.Suspend = req.Suspend
	if err := c.Update(ctx, &sched); err != nil {
		c.Logger.Error("broodschedules suspend update failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.reply(bus.BroodScheduleResponse{Error: err.Error()})
	}

	specRaw, _ := json.Marshal(sched.Spec)
	statusRaw, _ := json.Marshal(sched.Status)
	return c.reply(bus.BroodScheduleResponse{
		Namespace: sched.Namespace,
		Name:      sched.Name,
		Spec:      specRaw,
		Status:    statusRaw,
	})
}
