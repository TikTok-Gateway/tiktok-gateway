package libtiktok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	pb "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok/pb"
	"google.golang.org/protobuf/encoding/protowire"
	"strconv"
)

// Protocol derived from TikTok web SDK CreateConversationV2, October 2026.
// A successful create is only a preflight; send permission can still change.
type MessagingProfile struct {
	ID          string
	Username    string
	FollowsSelf bool
	Blocked     bool
	Deleted     bool
}

func (c *Client) MessagingProfile(ctx context.Context, id string) (MessagingProfile, error) {
	ids, _ := json.Marshal([]string{id})
	r, e := c.r.R().SetContext(ctx).SetQueryParams(map[string]string{"aid": "1988", "user_ids": string(ids)}).Get("/tiktok/v1/im/user/profile/")
	if e != nil || r.StatusCode() != 200 {
		return MessagingProfile{}, fmt.Errorf("profile request failed")
	}
	var d struct {
		Status *int `json:"status_code"`
		Users  []struct {
			Profile struct {
				ID       string `json:"user_id_str"`
				Name     string `json:"unique_id"`
				Follower int    `json:"follower_status"`
				Deleted  int    `json:"deleted"`
				Block    struct {
					Block   bool `json:"block"`
					Blocked bool `json:"blocked_by"`
				} `json:"block_info"`
			} `json:"im_user_profile"`
		} `json:"users"`
	}
	if json.Unmarshal(r.Body(), &d) != nil || d.Status == nil || *d.Status != 0 || len(d.Users) != 1 || d.Users[0].Profile.ID != id {
		return MessagingProfile{}, fmt.Errorf("profile response invalid")
	}
	p := d.Users[0].Profile
	return MessagingProfile{id, p.Name, p.Follower == 1, p.Block.Block || p.Block.Blocked, p.Deleted != 0}, nil
}
func wireInt(b []byte, field protowire.Number) (uint64, error) {
	for len(b) > 0 {
		n, t, k := protowire.ConsumeTag(b)
		if k < 0 {
			return 0, fmt.Errorf("invalid protobuf")
		}
		b = b[k:]
		l := protowire.ConsumeFieldValue(n, t, b)
		if l < 0 {
			return 0, fmt.Errorf("invalid protobuf")
		}
		if n == field {
			if t != protowire.VarintType {
				return 0, fmt.Errorf("wrong protobuf type")
			}
			v, _ := protowire.ConsumeVarint(b)
			return v, nil
		}
		b = b[l:]
	}
	return 0, nil
}
func wInt(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func wBytes(b []byte, n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, n, protowire.BytesType), v)
}

type MessagingDenied struct{ Status, Check uint64 }

func (e *MessagingDenied) Error() string {
	return fmt.Sprintf("messaging rejected: status=%d check=%d", e.Status, e.Check)
}
func (c *Client) PrepareDirectConversation(ctx context.Context, selfID, userID string) (*Conversation, error) {
	a, e := strconv.ParseUint(selfID, 10, 64)
	if e != nil {
		return nil, e
	}
	b, e := strconv.ParseUint(userID, 10, 64)
	if e != nil || a == b {
		return nil, fmt.Errorf("invalid participants")
	}
	data, e := c.getMessagesUniversalDataContext(ctx)
	if e != nil {
		return nil, fmt.Errorf("session unavailable")
	}
	ac, e := data.getAppContext()
	if e != nil {
		return nil, e
	}
	device, ok := ac["wid"].(string)
	if !ok {
		return nil, fmt.Errorf("missing device")
	}
	key, e := generateP256Key()
	if e != nil {
		return nil, e
	}
	pub := base64.StdEncoding.EncodeToString(p256UncompressedPoint(key))
	path := "/v2/conversation/create"
	proof, e := buildDPoPJWT(key, "POST", "https://im-api.tiktok.com"+path)
	if e != nil {
		return nil, e
	}
	cookie := c.rIA.Header.Get("Cookie")
	ms := extractCookie(cookie, "msToken")
	fp := extractCookie(cookie, "s_v_web_id")
	body := wInt(nil, 1, 1)
	body = wInt(body, 2, a)
	body = wInt(body, 2, b)
	payload := &pb.SendRequestPayload{}
	payload.ProtoReflect().SetUnknown(wBytes(nil, 609, body))
	req := &pb.SendRequest{MessageType: protoUint64(609), SubCommand: protoUint64(10021), ClientVersion: protoString("1.6.0"), Options: emptyProtoMessage(), PlatformFlag: protoUint64(3), Reserved_6: protoUint64(0), GitHash: protoString(""), DeviceId: protoString(device), ClientPlatform: protoString("web"), Metadata: metadataKVsToProto(buildSendMetadata(device, ms, fp, pub)), FinalFlag: protoUint64(1), Payload: payload}
	encoded, e := marshalProto(req)
	if e != nil {
		return nil, e
	}
	resp, e := c.rIA.R().SetContext(ctx).SetHeader("Content-Type", "application/x-protobuf").SetHeader("Origin", "https://www.tiktok.com").SetHeader("tt-ticket-guard-public-key", pub).SetHeader("tt-ticket-guard-version", "2").SetHeader("tt-ticket-guard-web-version", "1").SetHeader("tt-ticket-guard-iteration-version", "0").SetQueryParams(map[string]string{"aid": imAID, "version_code": "1.0.0", "app_name": "tiktok_web", "device_platform": "web_pc", "ztca-version": "1", "ztca-dpop": proof, "msToken": ms, "X-Bogus": randomBogus()}).SetBody(encoded).Post(path)
	if e != nil || resp.StatusCode() != 200 {
		return nil, fmt.Errorf("conversation request failed")
	}
	var envelope pb.InboxResponse
	if e = unmarshalProto(resp.Body(), &envelope); e != nil {
		return nil, e
	}
	if envelope.GetStatus() != 0 {
		return nil, &MessagingDenied{Status: envelope.GetStatus()}
	}
	outer, ok := extractLengthDelimitedField(resp.Body(), 6)
	if !ok {
		return nil, fmt.Errorf("missing conversation body")
	}
	inner, ok := extractLengthDelimitedField(outer, 609)
	if !ok {
		return nil, fmt.Errorf("missing create result")
	}
	status, e := wireInt(inner, 5)
	if e != nil {
		return nil, e
	}
	check, e := wireInt(inner, 2)
	if e != nil {
		return nil, e
	}
	if status != 0 || check != 0 {
		return nil, &MessagingDenied{status, check}
	}
	info, ok := extractLengthDelimitedField(inner, 1)
	if !ok {
		return nil, fmt.Errorf("missing conversation")
	}
	id, ok := extractLengthDelimitedField(info, 1)
	if !ok {
		return nil, fmt.Errorf("missing conversation id")
	}
	short, e := wireInt(info, 2)
	if e != nil || short == 0 {
		return nil, fmt.Errorf("missing conversation short id")
	}
	kind, e := wireInt(info, 3)
	if e != nil || kind != 1 {
		return nil, fmt.Errorf("not a direct conversation")
	}
	// TikTok's deterministic DM ID binds both participants; never trust a different target.
	expected1 := "0:1:" + selfID + ":" + userID
	expected2 := "0:1:" + userID + ":" + selfID
	if string(id) != expected1 && string(id) != expected2 {
		return nil, fmt.Errorf("conversation participant mismatch")
	}
	return &Conversation{ID: string(id), SourceID: short, ConversationType: 1, Participants: []string{selfID, userID}}, nil
}

// Sticker intentionally excludes sender and conversation IDs. Only a sticker-only
// attachment from the account owner's test message can become a template.
type Sticker struct{ attachment []byte }

func NewSticker(m Message) (*Sticker, error) {
	if m.MessageSubtype != "sticker" || len(m.RawAttachment) == 0 {
		return nil, fmt.Errorf("not a sticker")
	}
	b := m.RawAttachment
	n, t, k := protowire.ConsumeTag(b)
	if k < 0 || n != 7 || t != protowire.BytesType {
		return nil, fmt.Errorf("unexpected sticker attachment")
	}
	value, l := protowire.ConsumeBytes(b[k:])
	if l < 0 || len(value) == 0 || k+l != len(b) {
		return nil, fmt.Errorf("mixed sticker attachment")
	}
	return &Sticker{attachment: append([]byte(nil), b...)}, nil
}
