/*This file is part of kuberpult.

Kuberpult is free software: you can redistribute it and/or modify
it under the terms of the Expat(MIT) License as published by
the Free Software Foundation.

Kuberpult is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
MIT License for more details.

You should have received a copy of the MIT License
along with kuberpult. If not, see <https://directory.fsf.org/wiki/License:Expat>.

Copyright 2023 freiheit.com*/

// Setup implementation shared between all microservices.
// If this file is changed it will affect _all_ microservices in the monorepo (and this
// is deliberately so).
package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Health uint

const (
	HealthStarting Health = iota
	HealthReady
	HealthFailed
)

type WatchDogPing struct {
	pingTime time.Time
	watchDog *watchDog
}

func (p *WatchDogPing) Pong() {
	p.watchDog.gotPing(p.pingTime)
}

type watchDog struct {
	ch          chan *WatchDogPing
	cancel      context.CancelFunc
	latency     time.Duration
	missedPings uint
}

func (w *watchDog) gotPing(t time.Time) {
	w.latency = time.Now().Sub(t)
	w.missedPings = 0
}

func (w *watchDog) missedPing() {
	w.missedPings = w.missedPings + 1
}

func newWatchDog(timeout time.Duration) *watchDog {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan *WatchDogPing, 1)
	wd := &watchDog{ch: ch, cancel: cancel}
	go func() {
		defer cancel()
		ticker := time.Tick(timeout)
		for {
			select {
			case t := <-ticker:
				select {
				case ch <- &WatchDogPing{t, wd}:
				default:
					wd.missedPing()
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return wd
}

type HealthReporter struct {
	server   *HealthServer
	name     string
	watchDog *watchDog
}

type report struct {
	Health  Health    `json:"health"`
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

func (r *HealthReporter) ReportReady(message string) {
	if r == nil {
		return
	}
	r.ReportHealth(HealthReady, message)
}

func (r *HealthReporter) ReportHealth(health Health, message string) {
	if r == nil {
		return
	}
	r.server.mx.Lock()
	defer r.server.mx.Unlock()
	if r.server.parts == nil {
		r.server.parts = map[string]report{}
	}
	r.server.parts[r.name] = report{
		Health:  health,
		Time:    r.server.now(),
		Message: message,
	}
}

func (r *HealthReporter) StartWatchDog(timeout time.Duration) (<-chan *WatchDogPing, context.CancelFunc) {
	wd := newWatchDog(timeout)
	if r != nil {
		r.watchDog = wd
	}
	return wd.ch, wd.cancel
}

type HealthServer struct {
	parts map[string]report
	mx    sync.Mutex
	clock func() time.Time
}

func (h *HealthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reports := h.reports()
	success := true
	for _, r := range reports {
		if r.Health != HealthReady {
			success = false
		}
	}
	body, err := json.Marshal(reports)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if success {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
	fmt.Fprint(w, string(body))
}

func (h *HealthServer) IsReady(name string) bool {
	h.mx.Lock()
	defer h.mx.Unlock()
	if h.parts == nil {
		return false
	}
	report := h.parts[name]
	return report.Health == HealthReady
}

func (h *HealthServer) reports() map[string]report {
	h.mx.Lock()
	defer h.mx.Unlock()
	result := make(map[string]report, len(h.parts))
	for k, v := range h.parts {
		result[k] = v
	}
	return result
}

func (h *HealthServer) now() time.Time {
	if h.clock == nil {
		return time.Now()
	}
	return h.clock()
}

func (h *HealthServer) Reporter(name string) *HealthReporter {
	r := &HealthReporter{
		server: h,
		name:   name,
	}
	r.ReportHealth(HealthStarting, "starting")
	return r
}

var (
	_ http.Handler = (*HealthServer)(nil)
)
