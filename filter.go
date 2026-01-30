package monitor

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type BaseFilter struct {
	Included    []string
	Excluded    []string
	IncludedIPs []string
	ExcludedIPs []string
}

func (f *BaseFilter) match(uri string, patterns []string) bool {
	if len(patterns) == 0 {
		return true // Empty list implies match all (for whitelist) or no match (for blacklist context, handled by caller)
	}
	for _, pattern := range patterns {
		if strings.Contains(pattern, "*") {
			if matched, _ := doublestar.Match(pattern, uri); matched {
				return true
			}
		} else {
			if strings.HasPrefix(uri, pattern) {
				return true
			}
		}
	}
	return false
}

func (f *BaseFilter) ShouldFilter(tr TracingDetails) bool {
	// 默认接收全部 (当没有配置规则时)
	if len(f.Included) == 0 && len(f.Excluded) == 0 && len(f.IncludedIPs) == 0 && len(f.ExcludedIPs) == 0 {
		return true
	}

	// 1. Check IP rules first
	ip := tr.ClientIP
	// IP whitelist (IncludedIPs)
	ipMatched := len(f.IncludedIPs) == 0
	for _, item := range f.IncludedIPs {
		if strings.Contains(item, "*") {
			if matched, _ := doublestar.Match(item, ip); matched {
				ipMatched = true
				break
			}
		} else {
			if ip == item {
				ipMatched = true
				break
			}
		}
	}

	if !ipMatched {
		return false
	}

	// IP blacklist (ExcludedIPs)
	for _, item := range f.ExcludedIPs {
		if strings.Contains(item, "*") {
			if matched, _ := doublestar.Match(item, ip); matched {
				return false
			}
		} else {
			if ip == item {
				return false
			}
		}
	}

	// 2. Check URI rules
	uri := tr.Uri

	// URI whitelist (Included)
	uriMatched := len(f.Included) == 0
	if !uriMatched {
		uriMatched = f.match(uri, f.Included)
	}

	if uriMatched {
		// URI blacklist (Excluded)
		if len(f.Excluded) > 0 {
			if f.match(uri, f.Excluded) {
				return false
			}
		}
	}

	return uriMatched
}
