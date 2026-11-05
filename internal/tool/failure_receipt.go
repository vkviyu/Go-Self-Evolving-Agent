package tool

import "errors"

// failureReceipt stores only errors the adapter returns as an ordinary model
// tool result. Infrastructure errors and unknown effects remain fatal and do
// not acquire replay permission by being recorded as tool.failed.
type failureReceipt struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Wrapped bool   `json:"wrapped,omitempty"`
}

func recoverableFailure(cause error) *failureReceipt {
	var fatal interface{ ToolExecutionFatal() bool }
	if errors.As(cause, &fatal) && fatal.ToolExecutionFatal() {
		return nil
	}
	receipt := &failureReceipt{Version: 1, Message: cause.Error()}
	switch {
	case onlyFailureCause(cause, ErrApprovalDenied):
		receipt.Kind = "approval_denied"
	case onlyFailureCause(cause, ErrPolicyDenied):
		receipt.Kind = "policy_denied"
	default:
		if precondition, ok := errors.AsType[*PreconditionError](cause); ok {
			receipt.Kind, receipt.Code, receipt.Message = "precondition", precondition.Code, precondition.Message
			_, receipt.Wrapped = errors.AsType[*ExecutionError](cause)
		} else if execution, ok := errors.AsType[*ExecutionError](cause); ok {
			receipt.Kind, receipt.Message = "execution", execution.Err.Error()
		} else {
			return nil
		}
	}
	return receipt
}

// A joined audit/infrastructure failure must never be downgraded to a denial.
// This matches the adapter's rule for exposing denials as model tool results.
func onlyFailureCause(cause, target error) bool {
	if cause == target {
		return true
	}
	if joined, ok := cause.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyFailureCause(child, target) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := cause.(interface{ Unwrap() error }); ok {
		return onlyFailureCause(wrapped.Unwrap(), target)
	}
	return false
}

type replayedDenial struct {
	message string
	cause   error
}

func (err *replayedDenial) Error() string { return err.message }
func (err *replayedDenial) Unwrap() error { return err.cause }

func (receipt failureReceipt) restore(binding Binding) (error, bool) {
	if receipt.Version != 1 || receipt.Message == "" {
		return nil, false
	}
	switch receipt.Kind {
	case "approval_denied":
		return &replayedDenial{message: receipt.Message, cause: ErrApprovalDenied}, true
	case "policy_denied":
		return &replayedDenial{message: receipt.Message, cause: ErrPolicyDenied}, true
	case "precondition":
		if receipt.Code == "" {
			return nil, false
		}
		failure := error(&PreconditionError{Code: receipt.Code, Message: receipt.Message})
		if receipt.Wrapped {
			failure = &ExecutionError{Tool: binding.Descriptor.Name + "@" + binding.Descriptor.Revision, Err: failure}
		}
		return failure, true
	case "execution":
		return &ExecutionError{Tool: binding.Descriptor.Name + "@" + binding.Descriptor.Revision, Err: errors.New(receipt.Message)}, true
	default:
		return nil, false
	}
}
