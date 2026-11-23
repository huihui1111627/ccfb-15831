package sim

import "kilnalert/internal/channel"

// DescribeChannel renders a channel's simulation parameters.
func DescribeChannel(c channel.Config) string {
	return channel.DescribeChannel(c)
}
