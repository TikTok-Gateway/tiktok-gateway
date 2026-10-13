package libtiktok

import (
	"context"
	"encoding/json"
	"fmt"
)

// Group 7 was observed in Firefox when selecting Activity > Followers.
// is_mark_read=0 keeps the request read-only.
func (c *Client) GetFollowerNoticesRaw(ctx context.Context) (map[string]any, error) {
	return c.GetFollowerNoticesPage(ctx, 0)
}

func (c *Client) GetFollowerNoticesPage(ctx context.Context, maxTime int64) (map[string]any, error) {
	resp, err := c.r.R().SetContext(ctx).
		SetHeader("Referer", "https://www.tiktok.com/").
		SetQueryParams(map[string]string{
			"aid": "1988", "app_name": "tiktok_web", "app_language": "ru-RU",
			"device_platform": "web_pc", "group_list": fmt.Sprintf(`[{"count":20,"is_mark_read":0,"group":7,"max_time":%d,"min_time":0}]`, maxTime),
		}).Get("/api/notice/multi/")
	if err != nil {
		return nil, fmt.Errorf("follower request failed")
	}
	if err := c.checkResponse("followers", resp); err != nil {
		return nil, err
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("follower HTTP status %d", resp.StatusCode())
	}
	var data map[string]any
	if json.Unmarshal(resp.Body(), &data) != nil {
		return nil, fmt.Errorf("follower response is not JSON")
	}
	if status, ok := data["status_code"].(float64); !ok || status != 0 {
		return nil, fmt.Errorf("follower API did not report success")
	}
	return data, nil
}
