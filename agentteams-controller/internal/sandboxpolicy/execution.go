package sandboxpolicy

import (
	"fmt"
	"math/big"
	"net/netip"
	"regexp"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

var deepAgentsDurationPartPattern = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)([hms])`)

// ResolveDuration validates the duration grammar consumed by the DeepAgents
// runtime, or returns fallback when the policy field is omitted. Accepted
// values consist only of lowercase h/m/s parts and must resolve exactly to a
// positive whole number of seconds. Parsing with rationals avoids silently
// rounding a fractional result.
func ResolveDuration(raw string, fallback time.Duration, field string) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	parts := deepAgentsDurationPartPattern.FindAllStringSubmatchIndex(raw, -1)
	totalSeconds := new(big.Rat)
	position := 0
	for _, part := range parts {
		if part[0] != position {
			return 0, invalidDurationError(field)
		}
		amount, ok := new(big.Rat).SetString(raw[part[2]:part[3]])
		if !ok {
			return 0, invalidDurationError(field)
		}
		unitSeconds := int64(1)
		switch raw[part[4]:part[5]] {
		case "h":
			unitSeconds = 60 * 60
		case "m":
			unitSeconds = 60
		}
		amount.Mul(amount, big.NewRat(unitSeconds, 1))
		totalSeconds.Add(totalSeconds, amount)
		position = part[1]
	}
	if position != len(raw) || totalSeconds.Sign() <= 0 || totalSeconds.Denom().Cmp(big.NewInt(1)) != 0 ||
		!totalSeconds.Num().IsInt64() {
		return 0, invalidDurationError(field)
	}
	seconds := totalSeconds.Num().Int64()
	maxSeconds := int64((time.Duration(1<<63 - 1)) / time.Second)
	if seconds > maxSeconds {
		return 0, invalidDurationError(field)
	}
	return time.Duration(seconds) * time.Second, nil
}

func invalidDurationError(field string) error {
	return fmt.Errorf("execution sandbox %s must use h, m, or s parts resolving to a positive whole number of seconds", field)
}

// ValidateExecutionDurations applies the shared Controller/runtime duration
// boundary before either a Worker or ExecutionSandbox workload is created.
func ValidateExecutionDurations(execution v1beta1.DeepAgentsExecutionConfig) error {
	if _, err := ResolveDuration(execution.IdleTimeout, 30*time.Minute, "idleTimeout"); err != nil {
		return err
	}
	if _, err := ResolveDuration(execution.MaxLifetime, 8*time.Hour, "maxLifetime"); err != nil {
		return err
	}
	return nil
}

// IntersectEgress validates both requested and configured ceiling rules and
// returns only CIDR/protocol/port combinations allowed by both sets.
func IntersectEgress(
	requested []v1beta1.DeepAgentsEgressRule,
	ceilings []v1beta1.DeepAgentsEgressRule,
) ([]v1beta1.DeepAgentsEgressRule, error) {
	requestedParsed, err := parseEgressRules(requested)
	if err != nil {
		return nil, fmt.Errorf("requested egress: %w", err)
	}
	ceilingParsed, err := parseEgressRules(ceilings)
	if err != nil {
		return nil, fmt.Errorf("egress ceiling: %w", err)
	}

	var result []v1beta1.DeepAgentsEgressRule
	seen := map[string]struct{}{}
	for _, request := range requestedParsed {
		for _, ceiling := range ceilingParsed {
			if request.protocol != ceiling.protocol {
				continue
			}
			intersection, ok := narrowerPrefix(request.prefix, ceiling.prefix)
			if !ok {
				continue
			}
			ports := intersectPorts(request.ports, ceiling.ports)
			if len(ports) == 0 {
				continue
			}
			key := intersection.String() + "/" + request.protocol
			for _, port := range ports {
				key += fmt.Sprintf("/%d", port)
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, v1beta1.DeepAgentsEgressRule{
				CIDR: intersection.String(), Protocol: request.protocol, Ports: ports,
			})
		}
	}
	return result, nil
}

type parsedEgressRule struct {
	prefix   netip.Prefix
	protocol string
	ports    []int32
}

func parseEgressRules(rules []v1beta1.DeepAgentsEgressRule) ([]parsedEgressRule, error) {
	parsed := make([]parsedEgressRule, 0, len(rules))
	for _, rule := range rules {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(rule.CIDR))
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", rule.CIDR, err)
		}
		prefix = prefix.Masked()
		protocol := strings.ToUpper(strings.TrimSpace(rule.Protocol))
		if protocol == "" {
			protocol = string(corev1.ProtocolTCP)
		}
		if protocol != string(corev1.ProtocolTCP) && protocol != string(corev1.ProtocolUDP) {
			return nil, fmt.Errorf("unsupported protocol %q", rule.Protocol)
		}
		if len(rule.Ports) == 0 {
			return nil, fmt.Errorf("CIDR %q must declare at least one port", rule.CIDR)
		}
		ports := append([]int32(nil), rule.Ports...)
		for _, port := range ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("invalid port %d for CIDR %q", port, rule.CIDR)
			}
		}
		parsed = append(parsed, parsedEgressRule{prefix: prefix, protocol: protocol, ports: ports})
	}
	return parsed, nil
}

func narrowerPrefix(a, b netip.Prefix) (netip.Prefix, bool) {
	if a.Addr().Is4() != b.Addr().Is4() {
		return netip.Prefix{}, false
	}
	if b.Contains(a.Addr()) && b.Bits() <= a.Bits() {
		return a, true
	}
	if a.Contains(b.Addr()) && a.Bits() <= b.Bits() {
		return b, true
	}
	return netip.Prefix{}, false
}

func intersectPorts(requested, ceilings []int32) []int32 {
	allowed := make(map[int32]struct{}, len(ceilings))
	for _, port := range ceilings {
		allowed[port] = struct{}{}
	}
	result := make([]int32, 0, len(requested))
	seen := map[int32]struct{}{}
	for _, port := range requested {
		if _, ok := allowed[port]; !ok {
			continue
		}
		if _, duplicate := seen[port]; duplicate {
			continue
		}
		seen[port] = struct{}{}
		result = append(result, port)
	}
	return result
}
