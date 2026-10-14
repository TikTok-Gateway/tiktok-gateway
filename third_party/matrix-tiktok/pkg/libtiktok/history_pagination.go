package libtiktok

import (
	"errors"
	pb "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok/pb"
	"google.golang.org/protobuf/encoding/protowire"
	"math"
	"strconv"
)

// TikTok web SDK MessagesInConversationResponseBody: field 2 is signed
// next_cursor; field 3 is has_more. The vendored generated message predates these
// fields, so decode its preserved unknown fields without editing generated code.
// Message field 25 is pre_conversation_index, NOT the response's next cursor:
// it may be -1 (encoded as MaxUint64) for the first message in a conversation.
func historyPagination(list *pb.ConversationMessageList) (string, bool, error) {
	var cursor uint64
	var more bool
	seenCursor, seenMore := false, false
	invalid := errors.New("invalid_history_pagination")
	b := list.ProtoReflect().GetUnknown()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			return "", false, invalid
		}
		b = b[k:]
		if n == 2 || n == 3 {
			if typ != protowire.VarintType {
				return "", false, invalid
			}
			value, k := protowire.ConsumeVarint(b)
			if k < 0 {
				return "", false, invalid
			}
			b = b[k:]
			if n == 2 {
				if seenCursor || value > math.MaxInt64 {
					return "", false, invalid
				}
				seenCursor = true
				cursor = value
			} else {
				if seenMore || value > 1 {
					return "", false, invalid
				}
				seenMore = true
				more = value == 1
			}
		} else {
			// Alternate encoded message rows must not look like empty history.
			if n == 4 {
				return "", false, errors.New("unsupported_history_encoding")
			}
			k := protowire.ConsumeFieldValue(n, typ, b)
			if k < 0 {
				return "", false, invalid
			}
			b = b[k:]
		}
	}
	if !seenCursor || !seenMore {
		return "", false, invalid
	}
	if cursor == 0 {
		if more || len(list.GetEntries()) > 0 {
			return "", false, invalid
		}
		return "", more, nil
	}
	return strconv.FormatUint(cursor, 10), more, nil
}
