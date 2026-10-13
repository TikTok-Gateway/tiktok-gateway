# Internal TikTok API notes — 2026-10-13

These are observations, not a supported API contract. Public TikTok web SDK code
was read from the scripts loaded by `https://www.tiktok.com/messages`.
The diagnostic copies live in ignored `.state/research`; never publish captured
account responses or cookies. No browser is driven by the daemon.

## Followers

`GET https://www.tiktok.com/api/notice/multi/`, group 7, `is_mark_read=0`.
Use `nid_str` and string `follow.from_user.uid`; never convert 64-bit IDs through
JSON floating-point numbers. `create_time` is seconds. Pagination uses `max_time`
with overlap and event-ID deduplication. Stop rather than ignore a page that fails
to advance. Maximum 20 pages per poll. The baseline reads one page and uses a
start timestamp to reject earlier events from later pages.

## Recipient and conversation

`GET /tiktok/v1/im/user/profile/` with `aid=1988` and JSON `user_ids`.
`im_user_profile.follower_status == 1` was confirmed for the test follower.
`block_info.block`, `block_info.blocked_by`, and `deleted` exclude recipients.
These flags are not a complete privacy permission preflight.

`POST https://im-api.tiktok.com/v2/conversation/create`, request command 609,
body field 609: conversation_type field 1 = 1, participants repeated field 2.
Response envelope body field 6 -> 609: conversation info field 1, check_code 2,
status 5. Conversation info ID 1, short ID 2, type 3. Validate the deterministic
DM ID against both intended participants before using the short ID to send.
Preparing an existing test conversation succeeded. Creating a previously absent
conversation has not been isolated as a controlled test.

## Send and verify

Send body format comes from the vendored library. Status decoding was corrected:
outer status field 3, inner body field 6 -> 100, inner status 3, check_code 5,
server_message_id 1. A nonzero status/check is a rejection even if an ID exists;
ID zero is never an acknowledgement. No automatic retries of message sends.

Recipient-specific codes from TikTok web client module: 7281, 7282, 7283, 7290,
7291, 7394. Code 7283 is receiver permission set to no one. Sender-specific codes
such as 7284, 7289, 7393, generic blocks 7170, and unknown codes stop the daemon.
Do not infer sender safety from a successful HTTP status.

The sticker template is the original owner's sticker message in the agreed test
conversation. Only an attachment consisting of protobuf field 7 is accepted;
no conversation or sender metadata is copied. Send awe type 1805, subtype sticker,
empty content JSON, same attachment. The template is reread periodically to refresh
attachment data. Every send is confirmed by exact server ID, sender and content/type
in the conversation history. Server history confirmation is not a read receipt.

## Failure semantics

Persist an attempted phase before sending. Crash or lost response may leave the
operation uncertain. Prefer a missed greeting to a duplicate: require inspection
rather than automatically resetting the attempted phase. A restart after confirmed
text may send only the remaining sticker. Session errors and recipient errors have
different effects. Never log raw API errors because they can contain signed URLs,
response bodies or session tokens.
