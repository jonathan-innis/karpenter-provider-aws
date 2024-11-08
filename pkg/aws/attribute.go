package sdk

import (
	"fmt"

	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
)

func toPrometheusLabels(props smithy.Properties) prometheus.Labels {
	return lo.MapEntries(props.Values(), func(k, v any) (string, string) {
		return str(k), str(v)
	})
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	} else if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprintf("%#v", v)
}
