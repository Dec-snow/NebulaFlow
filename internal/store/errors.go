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
)
