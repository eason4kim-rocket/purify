package discovery

import "sync"

const warningLimitCode = "warning_limit"

type warningCollector struct {
	mu        sync.Mutex
	maximum   int
	values    []Warning
	truncated bool
}

func newWarningCollector(maximum int) *warningCollector {
	return &warningCollector{maximum: maximum}
}

func (collector *warningCollector) add(warning Warning) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	reserve := collector.maximum - 1
	if reserve < 0 {
		reserve = 0
	}
	if len(collector.values) < reserve {
		collector.values = append(collector.values, warning)
		return
	}
	collector.truncated = true
	if reserve == 0 {
		return
	}
	worst := 0
	for index := 1; index < len(collector.values); index++ {
		if warningLess(collector.values[worst], collector.values[index]) {
			worst = index
		}
	}
	if warningLess(warning, collector.values[worst]) {
		collector.values[worst] = warning
	}
}

func (collector *warningCollector) list() []Warning {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	values := append([]Warning(nil), collector.values...)
	if collector.truncated {
		values = append(values, warningLimit())
	}
	return values
}

func warningLimit() Warning {
	return Warning{
		Source:  "discovery",
		Code:    warningLimitCode,
		Message: "additional discovery warnings were truncated",
	}
}

func limitWarnings(warnings []Warning, maximum int) []Warning {
	stable := stableWarnings(warnings)
	filtered := make([]Warning, 0, len(stable))
	truncated := false
	for _, warning := range stable {
		if warning.Code == warningLimitCode {
			truncated = true
			continue
		}
		filtered = append(filtered, warning)
	}
	if len(filtered) > maximum {
		filtered = filtered[:maximum]
		truncated = true
	}
	if !truncated {
		return filtered
	}
	if maximum <= 1 {
		return []Warning{warningLimit()}
	}
	if len(filtered) >= maximum {
		filtered = filtered[:maximum-1]
	}
	filtered = append(filtered, warningLimit())
	return stableWarnings(filtered)
}
