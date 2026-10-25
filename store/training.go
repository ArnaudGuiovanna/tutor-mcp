// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"tutor-mcp/models"
)

type TrainingStore interface {
	TrainingAction(context.Context, models.Principal, string, models.TrainingRequest) (any, error)
}

type conversationKey struct{}

func WithConversation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, conversationKey{}, id)
}

func ConversationID(ctx context.Context) string {
	id, _ := ctx.Value(conversationKey{}).(string)
	return id
}
