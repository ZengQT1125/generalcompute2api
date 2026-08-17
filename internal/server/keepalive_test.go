package server

import (
	"testing"
	"time"
)

func TestNextKeepAliveAt(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time // 已带 +08:00 时区
		at   string
		want time.Time // 期望的北京时间
	}{
		{
			name: "下午运行 -> 次日凌晨4点",
			now:  time.Date(2026, 8, 17, 16, 0, 0, 0, beijingLocation),
			at:   "04:00",
			want: time.Date(2026, 8, 18, 4, 0, 0, 0, beijingLocation),
		},
		{
			name: "凌晨3点运行 -> 当日4点",
			now:  time.Date(2026, 8, 17, 3, 0, 0, 0, beijingLocation),
			at:   "04:00",
			want: time.Date(2026, 8, 17, 4, 0, 0, 0, beijingLocation),
		},
		{
			name: "刚过4点 -> 次日4点",
			now:  time.Date(2026, 8, 17, 4, 0, 30, 0, beijingLocation),
			at:   "04:00",
			want: time.Date(2026, 8, 18, 4, 0, 0, 0, beijingLocation),
		},
		{
			name: "自定义时刻 23:30",
			now:  time.Date(2026, 8, 17, 10, 0, 0, 0, beijingLocation),
			at:   "23:30",
			want: time.Date(2026, 8, 17, 23, 30, 0, 0, beijingLocation),
		},
		{
			name: "非法配置回退默认 04:00",
			now:  time.Date(2026, 8, 17, 12, 0, 0, 0, beijingLocation),
			at:   "abc",
			want: time.Date(2026, 8, 18, 4, 0, 0, 0, beijingLocation),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextKeepAliveAt(c.now, c.at)
			if !got.Equal(c.want) {
				t.Fatalf("nextKeepAliveAt(%v, %q) = %v, want %v", c.now, c.at, got, c.want)
			}
		})
	}
}
