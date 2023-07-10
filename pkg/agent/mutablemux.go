package agent

import (
	"net/http"
	"sync/atomic"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

//
// mutableRouter wraps the mux router, and allows the router to be
// atomically changed.
//

type mutableRouter struct {
	logger *zap.Logger
	router atomic.Value // mux.Router
}

func newMutableRouter(logger *zap.Logger, handler *mux.Router) *mutableRouter {
	mr := mutableRouter{
		logger: logger.Named("mutable_router"),
	}
	mr.router.Store(handler)
	return &mr
}

func (mr *mutableRouter) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	// Atomically grab the underlying mux router and call it.
	routerValue := mr.router.Load()
	router, ok := routerValue.(*mux.Router)
	if !ok {
		mr.logger.Panic("invalid router type")
	}
	router.ServeHTTP(responseWriter, request)
}

func (mr *mutableRouter) updateRouter(newHandler *mux.Router) {
	mr.router.Store(newHandler)
}
