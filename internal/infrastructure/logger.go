package infrastructure

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
)

type PrettyHandlerOptions struct {
	SlogOpts  slog.HandlerOptions
	Multiline bool
}

type PrettyHandler struct {
	slog.Handler
	l         *log.Logger
	multiline bool
}

func (h *PrettyHandler) Handle(ctx context.Context, r slog.Record) error {
	level := r.Level.String() + ":"

	fields := make(map[string]interface{}, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})

	var (
		b   []byte
		err error
	)
	if h.multiline {
		b, err = json.MarshalIndent(fields, "", "  ")
	} else {
		b, err = json.Marshal(fields)
	}
	if err != nil {
		return err
	}

	timeStr := r.Time.Format("[15:05:05.000]")
	msg := r.Message

	h.l.Println(timeStr, level, msg, string(b))

	return nil
}

func NewPrettyHandler(
	out io.Writer,
	opts PrettyHandlerOptions,
) *PrettyHandler {
	h := &PrettyHandler{
		Handler:   slog.NewJSONHandler(out, &opts.SlogOpts),
		l:         log.New(out, "", 0),
		multiline: opts.Multiline,
	}

	return h
}
