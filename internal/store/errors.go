package store

import (
	"errors"
)

// 领域错误统一在此定义，API 层据此映射 HTTP 状态码。
var (
	ErrUserExists   = errors.New("username or email already exists")
	ErrUserNotFound = errors.New("user not found")

	ErrWorkflowNotFound = errors.New("workflow not found")
	ErrTaskNotFound     = errors.New("task not found")
	ErrKBNotFound       = errors.New("knowledge base not found")
	ErrDocNotFound      = errors.New("document not found")
	ErrProviderNotFound = errors.New("llm provider not found")

	// ErrValidation 表示参数校验失败（非法输入）。
	// 所有"调用方传错了参数"类的错误都应包装此错误，
	// 以便 API 层统一映射为 400 Bad Request。
	ErrValidation = errors.New("validation error")
)

// ValidationError 携带具体字段信息的校验错误。
// 使用方式：return fmt.Errorf("%w: %s", store.ErrValidation, msg)
// 或直接使用本类型以保留结构化字段信息。
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return "validation error: " + e.Field + ": " + e.Message
	}
	return "validation error: " + e.Message
}

func (e *ValidationError) Unwrap() error { return ErrValidation }
