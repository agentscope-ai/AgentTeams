package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type humanPasswordResetter interface {
	UserID(localpart string) string
	ResetUserPassword(ctx context.Context, userID, password string) error
}

func canResetHumanPassword(human *v1beta1.Human) bool {
	return human.Labels[v1beta1.LabelSystemAdmin] != "true" &&
		human.Spec.IdentitySource == nil && human.Status.MatrixUserID != "" && human.DeletionTimestamp.IsZero()
}

func (h *ResourceHandler) ResetHumanPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := authpkg.CallerFromContext(ctx)
	if caller == nil || caller.Role != authpkg.RoleAdmin {
		httputil.WriteError(w, http.StatusForbidden, "仅管理员可以重置用户密码")
		return
	}
	if !h.passwordResetEnabled || h.passwordResetter == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "当前部署未启用用户密码重置")
		return
	}

	var input struct {
		NewPassword string `json:"newPassword"`
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input)
	length := utf8.RuneCountInString(input.NewPassword)
	if err != nil || length < 8 || length > 128 || strings.TrimSpace(input.NewPassword) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "新密码需为 8–128 个字符")
		return
	}

	key := client.ObjectKey{Namespace: h.namespace, Name: r.PathValue("name")}
	var human v1beta1.Human
	if err := h.referenceReader().Get(ctx, key, &human); err != nil {
		writeK8sError(w, "get human for password reset", err)
		return
	}
	if h.referenceOutOfScope(human.Labels) {
		httputil.WriteError(w, http.StatusNotFound, "用户不存在")
		return
	}
	userID := human.Status.MatrixUserID
	if !canResetHumanPassword(&human) || userID != h.passwordResetter.UserID(human.Spec.EffectiveUsername(human.Name)) {
		httputil.WriteError(w, http.StatusConflict, "此账号无法在用户列表中重置密码")
		return
	}
	if err := h.passwordResetter.ResetUserPassword(ctx, userID, input.NewPassword); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "聊天服务未能确认密码重置")
		return
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1beta1.Human
		if err := h.referenceReader().Get(ctx, key, &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != human.UID || current.Status.MatrixUserID != userID || current.Status.InitialPassword == "" {
			return nil
		}
		current.Status.InitialPassword = ""
		return h.client.Status().Update(ctx, &current)
	}); err != nil {
		log.FromContext(ctx).Error(err, "password reset succeeded but the old initial password could not be cleared", "user", key.Name)
	}
	if h.audit != nil {
		h.audit.Record(ctx, audit.Event{Who: caller.Username, Role: caller.Role, Target: key.Name, Action: "human_password_reset"})
	}
	w.WriteHeader(http.StatusNoContent)
}
