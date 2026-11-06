package httpapi

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/logging"
)

// unlogged paths get no access line: probes and scrapes, every few seconds.
var unlogged = map[string]bool{"/health": true, "/ready": true, "/metrics": true}

// AccessLog is the outermost handler of every HTTP server (docs/logging-design.md, "The access
// line"). It gives the request its ID (as WithRequestID), an OpenTelemetry server span continuing
// any traceparent, and a logging scope; recovers a panic into a 500 and an error line (a re-panic
// would make net/http print a stack that isn't JSON); and writes one access line once the response
// is complete. The route is the scope's (service.Handler sets it) or else r.Pattern.
func AccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	tracer := otel.Tracer("github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestID(r)
		if r.Header.Get("X-Request-Id") != id {
			r = r.Clone(r.Context())
			r.Header.Set("X-Request-Id", id)
		}
		w.Header().Set("X-Request-Id", id)
		if unlogged[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer))
		scope := logging.NewScope(id)
		r = r.WithContext(ContextWithRequestID(logging.WithScope(ctx, scope), id))
		var body *countingBody
		if r.Body != nil && r.Body != http.NoBody {
			body = &countingBody{ReadCloser: r.Body}
			r.Body = body
		}
		aw := &accessWriter{ResponseWriter: w, scope: scope}
		defer func() {
			p := recover()
			if p != nil && p != http.ErrAbortHandler {
				logger.ErrorContext(r.Context(), "panic", "panic", fmt.Sprint(p))
				if !aw.wroteHeader {
					WriteError(aw, Internal(nil))
				}
				aw.status = http.StatusInternalServerError
			}
			route, caller, errorCode := scope.Finish()
			if route == "" {
				route = logging.RouteOf(r.Pattern)
			}
			status := aw.status
			switch {
			case status != 0:
			case r.Header.Get("Upgrade") != "":
				status = http.StatusSwitchingProtocols // hijacked: the handler wrote 101 itself
			case p != nil:
				status = http.StatusInternalServerError
			default:
				status = http.StatusOK // nothing written: net/http sends 200
			}
			requestBytes := int64(0)
			if body != nil {
				requestBytes = body.n
			}
			level, attrs := logging.AccessAttrs(logging.Access{
				Method: r.Method, Route: route, Status: status,
				DurationMs:   float64(time.Since(started).Microseconds()) / 1000,
				RequestBytes: requestBytes, ResponseBytes: aw.bytes,
				ClientIP: ClientIP(r), UserAgent: r.UserAgent(), Caller: caller, ErrorCode: errorCode,
			})
			logger.LogAttrs(r.Context(), level, "request", attrs...)
			span.SetName(r.Method + " " + route)
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			span.End()
			if p == http.ErrAbortHandler {
				panic(p)
			}
		}()
		next.ServeHTTP(aw, r)
	})
}

type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

// accessWriter captures the status and counts the body; Flush, FlushError and Unwrap keep streaming,
// deadlines and Hijack working.
type accessWriter struct {
	http.ResponseWriter
	scope       *logging.Scope
	status      int
	wroteHeader bool
	bytes       int64
}

func (w *accessWriter) WriteHeader(code int) {
	if !w.wroteHeader && code >= 200 {
		w.status, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *accessWriter) Flush() { _ = w.FlushError() }

// FlushError is what http.ResponseController.Flush calls, so a failed flush (a closed stream)
// reaches the handler.
func (w *accessWriter) FlushError() error {
	if !w.wroteHeader {
		w.status, w.wroteHeader = http.StatusOK, true
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *accessWriter) SetErrorCode(code string) { w.scope.SetErrorCode(code) }
