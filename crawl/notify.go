package crawl

import (
	"time"

	"github.com/use-agent/purify/webhook"
)

type notification struct {
	targetURL string
	secret    string
	event     *webhook.Event
}

func (service *Service) startNotifier(capacity int) {
	service.notifications = make(chan notification, capacity)
	service.notifyStop = make(chan struct{})
	service.notifyDone = make(chan struct{})
	go service.runNotifier()
}

func (service *Service) runNotifier() {
	defer close(service.notifyDone)
	for {
		select {
		case <-service.notifyStop:
			return
		default:
		}
		select {
		case <-service.notifyStop:
			return
		case queued := <-service.notifications:
			service.deliverNotification(queued)
		}
	}
}

func (service *Service) enqueueNotification(queued notification) bool {
	if service == nil || service.notifier == nil || queued.event == nil {
		return false
	}
	select {
	case <-service.notifyStop:
		return false
	default:
	}
	service.notifyMu.Lock()
	defer service.notifyMu.Unlock()
	// Keep page delivery best-effort and reserve one queue slot per retained
	// crawl job for its terminal event.
	if queued.event.Type == "crawl.page" && len(service.notifications) >= maximumMaxPages {
		return false
	}
	select {
	case service.notifications <- queued:
		return true
	default:
		return false
	}
}

func (service *Service) deliverNotification(queued notification) {
	defer func() { _ = recover() }()
	service.notifier.Notify(queued.targetURL, queued.secret, queued.event)
}

func (service *Service) stopNotifier() {
	if service.notifyStop == nil {
		return
	}
	close(service.notifyStop)
	// A custom notifier may ignore all cancellation conventions. Keep Close
	// bounded; at most the service's single notifier worker can remain blocked.
	select {
	case <-service.notifyDone:
	case <-time.After(100 * time.Millisecond):
	}
}
