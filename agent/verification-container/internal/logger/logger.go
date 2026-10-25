package logger

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

func NewLogger(output io.Writer, token string) *slog.Logger {
	return slog.New(
		slog.NewJSONHandler(output, &slog.HandlerOptions{ReplaceAttr: func(_ []string, attribute slog.Attr) slog.Attr {
			if token == "" {
				return attribute
			}
			switch attribute.Value.Kind() {
			case slog.KindString:
				attribute.Value = slog.StringValue(strings.ReplaceAll(attribute.Value.String(), token, "[REDACTED]"))
			case slog.KindAny:
				value := fmt.Sprint(attribute.Value.Any())
				if strings.Contains(value, token) {
					attribute.Value = slog.StringValue(strings.ReplaceAll(value, token, "[REDACTED]"))
				}
			}
			return attribute
		}}),
	)
}
