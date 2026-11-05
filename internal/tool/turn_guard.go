package tool

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// PreconditionError is a recoverable protocol failure, not an execution or
// infrastructure failure. No policy approval or executor ran for this call.
type PreconditionError struct {
	Code    string
	Message string
}

func (err *PreconditionError) Error() string { return err.Message }

// TurnGuard constrains a caller's workflow without granting tool permissions.
// Implementations must scope checks to their own trusted session and turn.
type TurnGuard interface {
	BeforeCall(context.Context, Call, Descriptor) error
	BeforeComplete(context.Context, session.ID, string) error
}

type turnGuardKey struct{}
type turnGuardLink struct {
	guard  TurnGuard
	parent *turnGuardLink
}

func WithTurnGuard(ctx context.Context, guard TurnGuard) context.Context {
	parent, _ := ctx.Value(turnGuardKey{}).(*turnGuardLink)
	return context.WithValue(ctx, turnGuardKey{}, &turnGuardLink{guard: guard, parent: parent})
}

func checkCallPreconditions(ctx context.Context, call Call, descriptor Descriptor) error {
	link, _ := ctx.Value(turnGuardKey{}).(*turnGuardLink)
	for ; link != nil; link = link.parent {
		if err := link.guard.BeforeCall(ctx, call, descriptor); err != nil {
			return err
		}
	}
	return nil
}

func CheckTurnCompletion(ctx context.Context, sessionID session.ID, turnID string) error {
	link, _ := ctx.Value(turnGuardKey{}).(*turnGuardLink)
	for ; link != nil; link = link.parent {
		if err := link.guard.BeforeComplete(ctx, sessionID, turnID); err != nil {
			return err
		}
	}
	return nil
}
