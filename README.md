# Open WebUI → OpenAI API proxy

Proxy **Go, chỉ dùng thư viện chuẩn**, quản lý **nhiều website Open WebUI khác nhau**, mỗi website có nhiều tài khoản. Đăng nhập bằng email/mật khẩu, giữ token trong RAM và chuyển tiếp API.

- **WebUI tích hợp** tại `http://127.0.0.1:8080/` — thêm/sửa/xóa website và tài khoản, kiểm tra kết nối, thử chat. Thay đổi lưu ngay, không cần restart.
- `GET /healthz` — liveness công khai, không kiểm tra upstream.
- `GET /v1/accounts` — cần Bearer key; chỉ trả ID, website, enabled và cooldown, không trả mật khẩu/token.
- `GET /v1/models` — danh sách model của **một website/tài khoản được chọn**, không gộp nhiều nguồn.
- `POST /v1/chat/completions` — JSON hoặc SSE (`stream: true`), giữ nguyên luồng sự kiện upstream.
- Chọn nguồn bằng header `X-Website-ID` và/hoặc `X-Account-ID`.
- Proxy đầu ra HTTP/HTTPS/SOCKS5: dùng chung, riêng từng website hoặc riêng từng tài khoản.
- OTA bằng CLI: kiểm tra, cập nhật binary có chữ ký Ed25519 và rollback; không cần tải mã nguồn/build lại tại máy chạy.

Open WebUI đích phải đang chạy; tài khoản cần có quyền dùng model. URL kho mã GitHub không phải URL Open WebUI; email/mật khẩu là của Open WebUI, không phải GitHub.

## 1. Cấu hình

Yêu cầu **Go 1.23+** hoặc Docker Compose. Không cần cài dependency; module dùng thư viện chuẩn. Docker build bằng Go 1.25.

```bash
cp .env.example .env
chmod 600 .env
openssl rand -hex 32   # PROXY_API_KEY (tối thiểu 5 ký tự, dài cho an toàn)
openssl rand -hex 32   # ADMIN_API_KEY (tối thiểu 5 ký tự, nên khác khóa proxy)
```

```dotenv
CONFIG_FILE=data/config.json
ADMIN_API_KEY=replace-with-a-separate-random-admin-key
PROXY_API_KEY=replace-with-a-random-key-at-least-24-characters
HOST=127.0.0.1
PORT=8080
UPSTREAM_TIMEOUT_MS=300000
ACCOUNT_COOLDOWN_SECONDS=60
OUTBOUND_PROXY=
```

Mở `http://127.0.0.1:8080/`, đăng nhập bằng `ADMIN_API_KEY`, rồi thêm website và tài khoản ngay trên WebUI. Lần lưu đầu tiên sẽ tạo `CONFIG_FILE` (mặc định `data/config.json`, quyền `0600`).

Nếu chưa có file cấu hình, proxy vẫn khởi động với danh sách rỗng để bạn thiết lập qua WebUI. Khi đó `/v1/*` trả `503` cho tới khi có ít nhất một tài khoản.

### Định dạng `CONFIG_FILE`

WebUI ghi file này tự động; có thể sửa tay khi proxy đang dừng:

```json
{
  "websites": [
    {"id":"site-a","name":"Open WebUI A","base_url":"https://webui-a.example.com","proxy_url":"socks5h://127.0.0.1:1080"},
    {"id":"site-b","name":"Open WebUI B","base_url":"https://webui-b.example.com/webui"}
  ],
  "accounts": [
    {"id":"acc-1","name":"Acc 1 · site A","website_id":"site-a","email":"you@example.com","password":"your-password"},
    {"id":"acc-2","name":"Acc 2 · site A","website_id":"site-a","email":"other@example.com","password":"other-password","proxy_url":"direct"},
    {"id":"acc-3","name":"Acc 3 · site B","website_id":"site-b","email":"third@example.com","password":"third-password"}
  ]
}
```

- `websites[]` mỗi phần tử là một Open WebUI độc lập: `id` (bắt buộc), `name` (tùy chọn, trống sẽ hiển thị ID), `base_url`, `proxy_url` tùy chọn, `disabled` tùy chọn, `timeout_ms` tùy chọn.
- `timeout_ms` (website, 1–3600000) đặt ngân sách upstream riêng cho website đó — dùng cho host chậm; bỏ trống hoặc `0` là kế thừa `UPSTREAM_TIMEOUT_MS`. Request đi qua nhiều acc thì lấy ngân sách lớn nhất trong các acc ứng viên, và health probe của acc đó cũng dùng ngân sách này. Sửa được trong WebUI ở form website.
- `accounts[]` gắn với một website qua `website_id`; `proxy_url` tùy chọn ghi đè proxy của website. `base_url` chỉ dùng cho tài khoản legacy không có `website_id`.
- `name` của tài khoản cũng tùy chọn; để trống sẽ dùng `id`. **Độ dài tối thiểu của `PROXY_API_KEY`/`ADMIN_API_KEY` là 5 ký tự** — đủ cho cài đặt nội bộ, nhưng hãy đặt khóa dài ngẫu nhiên và chạy sau HTTPS khi mở ra ngoài.
- ID (website và tài khoản) là duy nhất toàn cục, `1–64` ký tự ASCII: chữ, số, `.`, `_`, `-`. Không sửa ID sau khi tạo.
- URL có thể chứa tiền tố đường dẫn như `/webui`. Giữ xác minh TLS; dùng HTTPS khi upstream ở máy khác.
- Chạy trực tiếp sẽ tự đọc `.env` trong thư mục hiện tại; biến môi trường của tiến trình **ưu tiên hơn** `.env`. Parser hỗ trợ `KEY=value`, dấu nháy đơn/đôi và dòng comment bắt đầu bằng `#`; không nội suy `$VARIABLE`. Compose có quy tắc nội suy riêng: đặt giá trị nhạy cảm trong dấu nháy đơn nếu có `$`.
- Thay đổi qua WebUI có hiệu lực ngay cho request tiếp theo; không cần khởi động lại.
- Mô hình cũ vẫn dùng được: `ACCOUNTS_FILE` (mảng tài khoản, có thể có `base_url`) hoặc `OPENWEBUI_EMAIL`/`OPENWEBUI_PASSWORD`; cả hai chỉ được đọc khi **không** đặt `CONFIG_FILE`.
- `DEFAULT_MODEL` tùy chọn, là model ID dùng khi request không chỉ định model. Mặc định host `127.0.0.1`, port `8080`, timeout `300000` ms, cooldown `60` giây.

Đừng đưa `.env` hoặc `data/` lên Git hay gửi mật khẩu trong cuộc trò chuyện. Các file này đã được loại khỏi Git và Docker build context.

Key có thể ngắn từ 5 ký tự và tên hiển thị là tùy chọn (trống thì dùng ID); hãy đặt khóa ngẫu nhiên dài và chạy sau HTTPS khi mở ra ngoài.

## 2. Khởi động

### Chạy trực tiếp

```bash
go run .
```

Hoặc build binary:

```bash
go build -o openwebui-proxy .
./openwebui-proxy
```

Chạy từ thư mục chứa `.env` và `data/config.json`. Không cần bước install.

### Docker

Image dùng multi-stage build, runtime Alpine có CA certificates và chạy bằng user không phải root (UID/GID `10001`). Compose đọc `.env` và mount **thư mục ghi được** `./data:/app/data` vì WebUI ghi cấu hình vào đó; thông tin đăng nhập không được đóng gói vào image.

```bash
mkdir -p data
sudo chown 10001:10001 data
docker compose up -d --build
docker compose logs -f proxy
```

Nếu Open WebUI cũng chạy Docker, dùng tên service trên cùng network (ví dụ `http://open-webui:8080`) làm `base_url` của website, hoặc địa chỉ host mà container truy cập được. `127.0.0.1` trong container là chính container.

Compose cố định `HOST=0.0.0.0`, `PORT=8080`, `CONFIG_FILE=/app/data/config.json`; cổng host chỉ mở tại `127.0.0.1:8080`. Nếu sửa `.env`, chạy `docker compose up -d --force-recreate proxy` để nạp lại biến môi trường.

Muốn truy cập từ máy khác, đặt sau reverse proxy HTTPS. **Khóa WebUI (`ADMIN_API_KEY`) cho phép thêm/xóa website và tài khoản — không chia sẻ nó cùng khóa API.** Khóa API (`PROXY_API_KEY`) cho phép dùng toàn bộ pool tài khoản; chưa có quota/rate limit riêng theo client.

## 3. WebUI quản trị

Mở `http://127.0.0.1:8080/`, nhập `ADMIN_API_KEY`.

| Trang | Chức năng |
| --- | --- |
| Dashboard | Số website, tài khoản, trạng thái khả dụng, base URL API |
| Websites | Thêm/sửa/xóa website Open WebUI, URL, proxy riêng, bật/tắt |
| Accounts | Thêm/sửa/xóa acc theo website, mật khẩu để trống khi sửa = giữ nguyên, kiểm tra kết nối |
| Playground | Lấy model và chat streaming trực tiếp qua website/acc đã chọn để thử cấu hình |

- Khóa quản trị chỉ giữ trong bộ nhớ trình duyệt, không lưu vào localStorage/cookie/URL, và không được gửi lại trong dữ liệu (mật khẩu và proxy đã cấu hình luôn được ẩn).
- Không xóa được website khi còn tài khoản đang tham chiếu (lỗi `409`); chuyển hoặc xóa tài khoản trước.
- Nhiều người cùng sửa? Mỗi lần lưu được tuần tự hóa; lần lưu sau ghi trên bản mới nhất của server, nên tải lại trang trước khi sửa nếu thấy dữ liệu cũ.
- Không có endpoint cập nhật phần mềm qua HTTP: OTA vẫn chạy bằng CLI (xem mục 6).

API quản trị tương ứng (Bearer `ADMIN_API_KEY`, `Content-Type: application/json`):

```text
GET    /api/admin/state
GET    /api/admin/models              POST /api/admin/chat/completions
POST   /api/admin/websites            PUT/DELETE /api/admin/websites/{id}
POST   /api/admin/accounts            PUT/DELETE /api/admin/accounts/{id}
POST   /api/admin/accounts/{id}/test
```

## 4. Gọi API và chọn website/tài khoản

Thay `YOUR_PROXY_KEY` bằng `PROXY_API_KEY`.

```bash
# Liveness công khai
curl http://127.0.0.1:8080/healthz

# Xem ID, website và trạng thái cooldown (cần xác thực)
curl http://127.0.0.1:8080/v1/accounts \
  -H 'Authorization: Bearer YOUR_PROXY_KEY'

# Model của website A; response có header X-Website-ID và X-Account-ID
curl -i http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_PROXY_KEY' \
  -H 'X-Website-ID: site-a'

# Ghim cả website và acc, tắt failover
curl -i http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_PROXY_KEY' \
  -H 'X-Website-ID: site-a' \
  -H 'X-Account-ID: acc-1'
```

Khi quyền model khác nhau giữa các website, **ghim `X-Website-ID` (và `X-Account-ID` nếu cần) khi lấy model và chat**. Danh sách model không phải danh sách hợp nhất của mọi website.

```bash
# Chat JSON; ghim website A, acc do proxy chọn trong website đó
curl -i http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_PROXY_KEY' \
  -H 'X-Website-ID: site-a' \
  -H 'Content-Type: application/json' \
  -d '{"model":"YOUR_MODEL_ID","messages":[{"role":"user","content":"Xin chào!"}],"stream":false}'

# SSE
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_PROXY_KEY' \
  -H 'X-Account-ID: acc-3' \
  -H 'Content-Type: application/json' \
  -d '{"model":"YOUR_MODEL_ID","messages":[{"role":"user","content":"Giới thiệu ngắn về bạn"}],"stream":true}'
```

### Cache danh sách mô hình

Host chậm (một `/api/models` mất ~20s) không nên bắt client chờ mỗi lần.

```dotenv
MODEL_CACHE_SECONDS=300          # TTL; 0 = tắt cache
MODEL_CACHE_REFRESH_SECONDS      # chu kỳ làm mới nền; trống = TTL/2 (khuyến nghị); 0 = tắt hẳn
MODEL_CACHE_MAX_SECONDS=3600     # trần cho TTL tự nới; trống = MODEL_CACHE_SECONDS x 12
PROBE_TIMEOUT_MS=60000           # timeout riêng cho health probe; 0 = dùng UPSTREAM_TIMEOUT_MS
TRANSLATE_MISSING=1              # bộ chuyển đổi: chạy Responses/Anthropic Messages qua chat completions khi upstream thiếu chuẩn đó
DEBUG_LOG=1                      # log chẩn đoán: mỗi lời gọi upstream + đoạn thân lỗi (không log khoá/mật khẩu); 0 = tắt
```

- Chỉ cache `/v1/models`; chat/completions **không bao giờ** phát lại từ cache.
- Cache theo khóa `<website-id>/<account-id>` nên hai web trùng ID acc không lẫn nhau.
- **Không bao giờ trả danh sách thiếu**: nếu một candidate chưa có entry trong cache, proxy nạp nốt (song song) rồi mới trả hợp đầy đủ — trước đây entry thiếu bị bỏ qua im lặng nên danh sách đảo giữa hai website tuỳ acc nào đã warm.
- **`/v1/models` trả HỢP của mọi account ứng viên**, không chỉ account mà round-robin vừa chọn. Trước đây mỗi lần gọi lại thấy một website khác nhau vì danh sách chỉ lấy từ candidate đầu tiên. Lần gọi đầu (cache lạnh) nạp song song tất cả account; sau đó phục vụ từ cache. Ghim `X-Account-ID` thì vẫn chỉ trả model của account đó.
- Response có header `X-Model-Cache: hit|stale|stale-error|miss` và `X-Model-Cache-Age` (giây) để debug.
- **Hết TTL không bị chặn**: bản cũ được trả ngay với `stale` kèm cờ `X-Model-Cache-Age`, đồng thời một lần fetch nền chạy song song (một fetch cho mỗi acc, các request dồn dập không nhân lên). Request sau nhận `hit`.
- **Log lỗi refresh được giới hạn**: cùng một acc lỗi (ví dụ mật khẩu bị từ chối) chỉ in **một dòng mỗi 10 phút** kèm `consecutive=N`, và in dòng `recovered` khi hết lỗi — trước đây in mỗi 2,5 phút, một log thật có 195 dòng trùng nhau che hết mọi thứ khác. Xem lỗi hiện tại của acc ở Dashboard → Health.
- **TTL tự nới cho upstream đắt**: entry giữ lâu bằng `max(TTL, 30 × thời gian fetch)`, chặn trên bởi `MODEL_CACHE_MAX_SECONDS`. Danh sách 21s vì thế không bị gọi lại mỗi 5 phút; danh sách 0.8s vẫn theo TTL gốc.
- **Upstream lỗi**: danh sách cũ vẫn được phục vụ (`stale-error` + `X-Cache-Error: upstream-failed`) thay vì trả lỗi cho client.
- **Warm ngay khi khởi động**: proxy fetch model cho mọi account enabled trước khi phục vụ request, nên host chậm 21s được trả giá lúc boot (log sẽ thấy dòng refresh trong ~20s đầu) chứ không phải ở client đầu tiên.
- Refresher quét **danh sách account đã cấu hình**, không phải danh sách key đã có trong cache — trước đây account mới thêm (hoặc cache còn rỗng) không bao giờ được nạp trước khi có client hỏi.
- Account đang `failed` bị refresher bỏ qua (health loop lo việc hồi), tránh đập vào host đã biết là chết.
- Làm mới nền mặc định **bật** (chu kỳ = TTL/2, sàn 1 giây) — trước đây trống nghĩa là tắt, khiến cache phục vụ danh sách cũ vô hạn.
- Mỗi acc được làm mới bằng ngân sách timeout riêng của acc đó, nên một host chậm không làm cả vòng quét bị cắt.
- Admin: `GET/POST/DELETE /api/admin/model-cache`; WebUI có thẻ "Cache mô hình" ở Dashboard.

### Bộ chuyển đổi khi upstream thiếu chuẩn API (`TRANSLATE_MISSING`)

Một số upstream (ví dụ `kaethegpt.duckdns.org`) chỉ có chat completions: `/openai/responses` trả `404 not_found_error`, một số route trả `405`. Bật `TRANSLATE_MISSING=1` rồi khởi động lại, proxy sẽ **tự dịch tại biên** thay vì trả lỗi:

| Client gọi | Proxy chuyển thành | Trả về |
|---|---|---|
| `POST /v1/responses` | `POST /api/chat/completions` | object Responses (`output[]`, `output_text`, `function_call`) |
| `POST /v1/messages` | `POST /api/chat/completions` | object Anthropic Messages (`content[]`, `tool_use`, `usage.input_tokens`) |
| stream = true | SSE chat → SSE đích | event `response.output_text.delta` / `content_block_delta` + envelope `message_start`/`message_stop` |

- Chuyển đổi **không bao giờ nói dối**: field mà schema chat không chứa được (ví dụ `reasoning`, `thinking`, `store`, `metadata`, `context_management`) được liệt kê trong header `X-Compat-Dropped`, và `X-Compat-Mode` cho biết đang dịch. Không có header thì không dịch.
- `input` của Responses (string hoặc items) được map sang `messages`; `function_call`/`function_call_output` được replay đúng thứ tự để tool call của client vẫn chạy.
- Tool "hosted" của Responses (`web_search`, `file_search`, `code_interpreter`…) chạy bên vendor, không thể mang qua chat → trả `400` ngay thay vì âm thầm bỏ tool.
- Tắt mặc định: chỉ áp dụng cho `/v1/responses` và `/v1/messages`; mọi route khác vẫn passthrough nguyên vẹn.

**Giới hạn còn lại**: hosted tool của Responses (`web_search`…) trả `400`; reasoning/thinking không được replay (báo trong `X-Compat-Dropped`).

### Log chẩn đoán (`DEBUG_LOG`)

Bật `DEBUG_LOG=1` rồi khởi động lại. Log đi ra stderr **và** vào vòng đệm 300 dòng xem được ở Dashboard → thẻ “Log chẩn đoán” (API `GET /api/admin/logs`), không cần shell trên host.

Dashboard còn có thẻ **“Lưu lượng theo website”** (`GET /api/admin/usage`): số request thành công / lỗi theo từng website trong 60 phút gần nhất, thời điểm và model của request cuối, cùng **lý do lỗi cuối cùng** upstream trả về. Số liệu chỉ nằm trong bộ nhớ (không ghi đĩa) và mất khi khởi động lại.

- Mỗi lời gọi upstream một dòng: `debug: POST /api/chat/completions account=… website=… status=400 elapsed=…`.
- Khi upstream trả lỗi, in thêm **đoạn thân lỗi** (tối đa 1.5 KB, đã nén khoảng trắng) — chính là chỗ Open WebUI nói rõ vì sao trả `400`.
- Passthrough (Responses, tools, MCP, embeddings…) in ra danh sách key của body và **cảnh báo** khi gặp field mà `/openai/responses` của Open WebUI hay từ chối: `reasoning`, `include`, `store`, `metadata`, `parallel_tool_calls`, `previous_response_id`, `truncation`, `background`, `top_logprobs`, `tool_choice`, `service_tier`, `max_tool_calls`, `prompt_cache_key`.
- Không bao giờ log header `Authorization`, khoá proxy hay mật khẩu; body lỗi được lọc ký tự điều khiển trước khi ghi.
- Tắt mặc định: khi `DEBUG_LOG` không bật, proxy chỉ log lúc khởi động và lỗi nghiêm trọng.

### `{"detail":"You have no credits remaining..."}` từ client

Từ v0.28.0 proxy **chuyển tiếp nguyên văn lý do** upstream trả về: response lỗi có `error.upstream` và `error.message` dạng `"Open WebUI returned HTTP 400: You have no credits remaining. ..."`. Trước đây chỉ có `returned HTTP 400` nên hết credit trông giống hệt request sai. Ngoài ra lỗi credit/quota được coi là lỗi **của account**: account đó vào cooldown và request chuyển sang account khác (ghim `X-Account-ID` thì không chuyển, trả lỗi thật). Cooldown này là **theo từng model** (v0.29.0): Open WebUI gộp nhiều provider, nên một acc hết credit OpenAI vẫn phục vụ Claude bình thường — chặn cả acc sẽ làm mất các model còn sống. Với nhiều acc, acc đã từ chối model đó bị xếp **cuối** danh sách ứng viên (vẫn giữ làm phương án cuối để trả lỗi thật thay vì "không có acc").

### `debug: model-cache refresh failed ... Open WebUI login failed`

Mật khẩu/email của acc đó đã bị upstream từ chối (đăng nhập lại bằng tay sẽ thấy). Proxy chỉ in một dòng mỗi 10 phút kèm `consecutive=N`; sửa mật khẩu trong WebUI là dòng `recovered` xuất hiện. Đây **không phải** lỗi proxy.

### `debug: model-cache refresh failed` (connection timed out)

Upstream đang treo, không phải proxy treo. Mặc định mỗi acc có 300s ngân sách, nên một lần treo TCP kéo dài ~2m47s trước khi bị bỏ. Cache vẫn phục vụ bản cũ (`X-Model-Cache: stale-error`) nên client không bị chặn, nhưng log sẽ ồn.

- Đặt `timeout_ms` cho website đó (ví dụ `20000`) để lần treo sau bị cắt sau 20s thay vì gần 3 phút.
- Nếu host lúc được lúc không: giữ nguyên, chỉ khi log đầy mới cần siết. Đây là log mức `debug`, tắt `DEBUG_LOG` là hết.

### `POST /v1/messages` trả 400: `tools: List should have at least 1 item`

Client Anthropic gửi `"tools": []` (mảng rỗng) — Open WebUI từ chối. Không phải lỗi proxy: bỏ trường `tools` khi rỗng ở phía client, hoặc bật ít nhất một tool.

### `POST /v1/responses` trả 404 từ một website

Route Responses chỉ có trên upstream mount `/openai`. Đã probe: `kaethegpt.duckdns.org` trả `404 {"type":"not_found_error"}` cho `/openai/responses`, trong khi `ai.baselgovernance.org` trả `401` (route tồn tại). Khi client dùng ghim `X-Website-ID`, request sẽ dừng ở website đó và fail hẳn thay vì failover — đúng thiết kế. Không ghim, hoặc dùng website có Responses.

### `POST /v1/messages/count_tokens` trả 405 (Method Not Allowed)

Open WebUI **không** phục vụ `POST /api/v1/messages/count_tokens` (đã probe: 405 + `{"detail":"Method Not Allowed"}` trên cả hai host). Từ v0.13.0 proxy chuyển `/v1/messages/count_tokens` sang `/openai/messages/count_tokens`, nơi Open WebUI đặt bộ đếm token (`401` khi chưa auth = route tồn tại). Một số client chấp nhận 405 cho count_tokens và tự bỏ qua; muốn sạch log thì cập nhật proxy.

### Gặp `Open WebUI returned HTTP 400` từ editor (Cline/Roo/Copilot…)

Triệu chứng: client báo `502 … Open WebUI returned HTTP 400` khi gọi Responses API, có thể kèm “retried 5 times”. Nguyên nhân thường gặp **không phải** proxy: client gửi field mà bản Open WebUI của bạn không nhận, và upstream chỉ trả `400` trống.

1. Bật `DEBUG_LOG=1`, khởi động lại, tái hiện lỗi, rồi mở Dashboard → “Log chẩn đoán”. Dòng `WARNING /v1/responses carries [...]` chỉ đúng field gây lỗi, dòng `upstream error body …` cho biết upstream nói gì.
2. Field hay bị từ chối: `reasoning`, `include`, `store`, `metadata`, `parallel_tool_calls`, `previous_response_id`, `truncation`, `background`, `top_logprobs`, `tool_choice`, `service_tier`, `max_tool_calls`, `prompt_cache_key`. Bỏ tính năng tương ứng ở phía client (ví dụ tắt “reasoning effort” / “reasoning summary”).
3. Nếu client dùng **giao thức Responses** (`/v1/responses`): proxy chuyển tới `/openai/responses` của Open WebUI. Kiểm bản upstream có mount router đó không (`curl -s -o /dev/null -w '%{http_code}' $BASE/openai/responses` — `405` là có route, `404` là không). Không có thì chuyển client sang **Chat Completions** (`/v1` + model thường) là chắc ăn nhất.
4. Tools/MCP: proxy chỉ chuyển tiếp, quyền thực thi và cấu hình MCP nằm ở upstream — MCP chưa bật thì tool call sẽ lỗi ở đó, không phải ở proxy.

### Healthcheck theo tài khoản

Proxy tự probe từng acc định kỳ để loại acc chết/đăng nhập hỏng khỏi round-robin trước khi có request thật.

- `HEALTH_CHECK_SECONDS` (mặc định 300) đặt chu kỳ; `HEALTH_CHECK_DISABLED=1` tắt.
- Probe dùng đúng auth của acc (Open WebUI login hoặc LibreChat key tĩnh) vào một endpoint chỉ đọc (`/models`); ghi `status: ok|failed`, `latency_ms`, `checked_at`.
- Acc `failed` bị bỏ khỏi vòng chọn và báo rõ khi ghim: `503 Selected account failed its health check: …`; sẽ tự hồi khi probe lại đạt.
- **Kiểm tra thủ công luôn probe thật**: nút “Kiểm tra” trong WebUI (và `POST /api/admin/accounts/<key>/test`) xoá verdict đang cache, đăng nhập lại rồi gọi `/models`, nên một lần lỗi mạng tạm thời không còn làm acc “kẹt unavailable” suốt cooldown. Probe thủ công thành công thì xoá luôn login-block và cooldown.
- **Verdict cache không gây failover**: lỗi đọc từ cache (`Account login temporarily unavailable`, `Selected account is cooling down`, `Selected account failed its health check`) không đẩy request sang acc khác, vì acc đã chọn chưa hề được gọi. Chỉ lỗi thật từ upstream mới failover như mô tả ở mục 5.
- `GET /v1/health` (hoặc `?probe=1` để chạy ngay), `POST /v1/health/probe` → báo cả report; cùng cơ chế xác thực Bearer. Cả hai đều là probe “thật” như nút Kiểm tra.
- Admin `GET/POST /api/admin/health`; trường `health`, `cooldown_seconds` và `login_block_seconds` trong `/api/admin/state` để WebUI hiển thị badge “Health: lỗi” / “Chờ N giây” / “Chặn đăng nhập N giây”.
- Lỗi probe được làm sạch (không lộ host/token/mật khẩu).

### Routing theo prefix `<id>/<model>`

Bật `MODEL_PREFIX=1` thì danh sách model trả ID dạng `"<website-id>/<model>"` (hoặc `"<acc-id>/<model>"` nếu ghim acc), để một OpenAI client gọi `model: "ai/openai/gpt-5.6-luna"` là tự chọn đúng website/acc **mà không cần header** `X-Website-ID`/`X-Account-ID`.

- **Prefix mặc định là website id**: `MODEL_PREFIX=1` trả `"<website-id>/<model>"`; khi caller ghim `X-Account-ID` thì prefix đổi sang `<acc-id>`.
- **Mỗi model mang prefix của web sở hữu nó**, không phải prefix của account đầu tiên trong danh sách ứng viên. Trước đây model của web B bị gắn tên web A (`ai.baselgovernance.org/gpt-5.6-luna` trong khi chỉ `kaethegpt.duckdns.org/gpt-5.6-luna` tồn tại) và gọi vào sẽ 400 `Model not found`.
### LibreChat: hai kiểu credential, hai surface khác nhau

LibreChat không có một "API model" thẳng như Open WebUI. Có hai đường, và proxy hỗ trợ **cả hai**, chọn bằng kiểu credential của acc:

| Credential | Đi tới | Dùng được cho |
|---|---|---|
| `api_key` (Agent API key) | `/api/agents/v1/*` | **Agent** đã định nghĩa sẵn. Không gọi thẳng model. |
| `email` + `password` | Session web: `POST /api/auth/login` → JWT → `POST /api/agents/chat/<endpoint>` → `GET /api/agents/chat/stream/<streamId>` | **Model** bất kỳ trên instance, như giao diện web |

Hai kiểu **không thay thế nhau**: Agents API từ chối JWT session (`invalid_api_key`), còn route chat UI không nhận agent key. Không được khai cả hai trên một acc (config sẽ bị từ chối).

Session account cần biết:
- JWT sống **15 phút**; proxy tự gia hạn (ưu tiên `POST /api/auth/refresh` bằng cookie `refreshToken`, hết hạn mới login lại).
- Mọi request đều gửi **User-Agent giống browser** — LibreChat ghi violation nếu thiếu và đủ điểm sẽ **ban IP** (`BAN_DURATION`, mặc định 2 giờ).
- Login bị từ chối (403 ban / sai mật khẩu) thì acc bị **tạm dừng 30 phút và KHÔNG retry** — mỗi lần thử là thêm một violation.
- Lịch sử nhiều lượt được **làm phẳng thành transcript** gửi trong một lượt; proxy không giữ trạng thái hội thoại giữa các lần gọi.
- Route chat là `/api/agents/chat/<endpoint>` với `endpointType: "custom"` và `endpoint` suy ra từ model (`Deepseek`, `openAI`, `Anthropic`, `Google`) — **không** dùng `agents`/`agent_id`.

Shape body (lấy từ DevTools của UI thật):

```json
{"text":"…","sender":"User","clientTimestamp":"…","isCreatedByUser":true,
 "parentMessageId":"00000000-0000-0000-0000-000000000000","messageId":"<uuid>",
 "error":false,"endpoint":"Deepseek","endpointType":"custom","model":"deepseek-chat",
 "key":"never","modelDisplayLabel":"Deepseek","isTemporary":false,"isRegenerate":false,
 "isContinued":false,"timezone":"UTC","clientRequestId":"<uuid>"}
```

### Model Parameters (LibreChat)

`temperature`, `top_p`, `max_tokens`/`max_completion_tokens`, `frequency_penalty`, `presence_penalty`, `seed`, `stop` của client được chuyển vào `endpointOption.model_parameters` của body LibreChat — đó là kênh duy nhất mà agent build đọc rồi đẩy xuống SDK provider. Không set thì **không gửi** block, để upstream giữ default của nó.

### Ẩn model chết

Khi upstream trả lời rằng **provider không có credential** (`no_user_key`, `openAI API Key not provided`, `illegal_model_request`, `endpoint_models_not_loaded`, `api key not provided`), model đó bị coi là **không dùng được** trên deployment ấy: nó bị ẩn khỏi `/v1/models` trong `PROVIDER_COOLDOWN_SECONDS` (mặc định 30 phút) và lỗi trả về nói rõ nguyên nhân thay vì trông như lỗi tạm thời. Tắt bằng `HIDE_UNAVAILABLE_MODELS=0`.

Stream: SSE event name luôn là `message`; ngữ nghĩa nằm trong field `event` của payload — `on_message_delta` (có `delta.content[].text`), `on_token_usage` (token), và frame cuối `{"final":true,...}`. Frame đầu tiên là echo tin nhắn của chính người dùng nên **không** được tính vào câu trả lời. Bật `LIBRECHAT_STREAM_DEBUG=1` để dump payload thô khi upstream đổi format.
- `LIBRECHAT_ORIGIN` chỉ cần đặt khi instance bật kiểm `Origin` chặt (gửi kèm `Origin`/`Referer`).

Lấy agent key: LibreChat → **Settings → Data & Privacy → API keys → Agent API Keys** (role cần quyền **Remote Agents**). Health probe của acc dùng agent key gọi `/api/agents/v1/agents`; **401** ở đó nghĩa là key sai/bị thu hồi hoặc thiếu quyền.

- Prefix phải khớp ID của acc hoặc website **đã đăng ký**; model ID chứa slash (như `openai/gpt-5.6`) thì giữ nguyên. Một helper duy nhất (`stripRoutingPrefix`) làm việc này cho cả danh sách lẫn chat, nên không còn cảnh vừa thấy `site-a/openai/gpt-5` vừa thấy `site-a/gpt-5` (bản sau bị upstream trả `Model not found`).
- Header do caller gửi luôn thắng; prefix chỉ áp dụng khi chưa có header.
- Áp dụng cả `chat/completions`, `responses` và các body passthrough có trường `model`.

### Chuẩn API hỗ trợ (v0.3.0)

Proxy expose toàn bộ surface chính thức của Open WebUI qua cùng một khoá và cùng cơ chế chọn website/acc:

| Chuẩn | Endpoint phía proxy | Chuyển tới Open WebUI |
| --- | --- | --- |
| OpenAI | `GET /v1/models`, `POST /v1/chat/completions`, `POST /v1/embeddings(/{idx})`, `GET /v1/files/…`, `GET /v1/tools/…`, `GET /v1/mcp/…`, `GET /v1/knowledge/…`, `GET /v1/audio/…` | `/api/v1/…` (chat/models có xử lý riêng: chuẩn hoá model, streaming, chọn acc) |
| Anthropic Messages | `POST /v1/messages`, `POST /v1/messages/count_tokens` | `/api/v1/messages`, `/openai/messages/count_tokens` |
| OpenAI Responses | `POST /v1/responses`, `GET|POST /v1/responses/{id}` | `/openai/responses…` |
| Ollama native | `/ollama/api/*` (tags, chat, generate, embed, ps, version…) | `/ollama/api/*` verbatim |
| Upstream OpenAI router | `/openai/*` (models, chat/completions, audio/speech, responses) | verbatim |
| Admin/utility | `/api/version`, `/api/config`, `/api/chats/…`, `/api/v1/models/all`, `/api/v1/utils/pdf`, `/api/v1/retrieval/…` | `/api/…` verbatim |

- Responses API nằm ở `/openai/responses` trên upstream (router OpenAI mount tại prefix `/openai`); `/v1/responses` phía proxy được map sang đó, hỗ trợ cả non-stream và `stream: true` (SSE). Payload PHẢI dùng `input`, **không** dùng `messages` — gửi `messages` sẽ nhận lỗi từ provider. Lưu ý payload được chuyển nguyên văn, không dịch giữa chat-completions và responses.
- Path không thuộc allow-list trả `404 Endpoint not found` — proxy không blind-forward.
- Tool call và request có tác dụng phụ không failover sang acc khác sau khi đã gửi.

### ID tài khoản chỉ cần duy nhất trong từng website

Hai website có thể có tài khoản **cùng ID** (ví dụ `web-a/acc` và `web-b/acc`). Định danh toàn hệ thống là cặp `<website-id>/<account-id>`:

- `X-Account-ID` gửi kèm `X-Website-ID` là đủ; gửi **ID trần** khi nó tồn tại ở nhiều website sẽ trả `409` (mơ hồ) thay vì đoán bừa.
- Admin API nhận cả `/api/admin/accounts/<id>` và `/api/admin/accounts/<website>/<id>`; đường dẫn đầy đủ bắt buộc khi ID bị trùng.
- `MODEL_PREFIX=1` hiển thị `<website-id>/<model>` nên không cần header vẫn route đúng.
- `/v1/accounts` và `/api/admin/state` trả thêm trường `key` (`<website>/<id>`) để phân biệt.

### Backend LibreChat (Agents API)

Mỗi acc có thể trỏ tới **Open WebUI** (mặc định, email + mật khẩu) hoặc **LibreChat Agents API** (API key tĩnh). Khai báo trên WebUI qua ô "Loại backend", hoặc trong `CONFIG_FILE`:

```json
{"id":"agent1","website_id":"lc","backend":"librechat","api_key":"lc_agent_key_xxx"}
```

- LibreChat **chỉ dùng `api_key`**: acc `librechat` bị từ chối nếu có `email` hoặc `password`, và khi đổi backend sang `librechat` proxy tự xoá credential cũ khỏi config.
- Chọn LibreChat trong WebUI sẽ ẩn hẳn ô Email/Mật khẩu.
- ánh xạ: `/v1/chat/completions` → `/api/agents/v1/chat/completions`, `/v1/models` → `/api/agents/v1/models` (ID là `agent_…`), `/v1/responses` → `/api/agents/v1/responses`, `/v1/events*` → `/api/agents/v1/events*`, `/v1/agents*` và `/v1/skills*` → management API.
- `POST /v1/events` **bắt buộc** header `Idempotency-Key` (1–256 ký tự `A-Za-z0-9._~:/+=-`), được chuyển tiếp nguyên vẹn; thiếu/sai → `400` trước khi gọi upstream.
- Không có bước login/refresh: key tĩnh gửi thẳng mỗi request, `401` **không** tự đăng nhập lại (an toàn cho events).
- Management routes cần machine OIDC token của LibreChat — proxy chỉ chuyển tiếp `Authorization` của acc, nên cần dùng API key hoặc mở `remoteApi` phía LibreChat.
- LibreChat Agents API đang **beta**, đường dẫn/payload có thể đổi giữa các bản.
- Không xác minh runtime với instance LibreChat thật nào trong phiên này.

### Tool calling, Web Search, MCP và embeddings

Proxy chuyển tiếp nguyên vẹn tham số OpenAI nên các tính năng của Open WebUI hoạt động bình thường:

```bash
# Tool calling / function calling
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_PROXY_KEY' -H 'Content-Type: application/json' \
  -d '{"model":"MODEL","messages":[{"role":"user","content":"Tính 7+5"}],
       "tools":[{"type":"function","function":{"name":"calc","parameters":{"type":"object"}}}],
       "tool_choice":"auto",
       "features":{"web_search":true,"code_interpreter":true,"image_generation":false}}'
```

| Endpoint | Chuyển tới Open WebUI | Ghi chú |
| --- | --- | --- |
| `POST /v1/chat/completions` | `/api/chat/completions` | `tools`, `tool_choice`, `features.web_search`, `code_interpreter`, `image_generation`, `files`, `metadata` được giữ nguyên |
| `POST /v1/embeddings` | `/api/embeddings` | Phụ thuộc embedding engine của Open WebUI |
| `GET /v1/tools/list` | `/api/v1/tools/list` | Danh sách tool; cần quyền Workspace → Tools |
| `GET /v1/mcp` | `/api/v1/mcp/servers` | Danh sách MCP server (phiên bản Open WebUI có MCP) |
| `/v1/knowledge/…`, `/v1/files/…` | `/api/v1/…` | RAG / knowledge / file của Open WebUI |

- Tool execution, web search, MCP authorization và RAG đều do **Open WebUI** thực hiện; proxy chỉ định tuyến website/acc, giới hạn body 10 MiB và ẩn lỗi nội bộ.
- Request tool **không tự failover sang acc khác** sau khi đã gửi, vì tool call có thể gây tác dụng phụ.
- Endpoint chưa tồn tại trên upstream (ví dụ `/v1/mcp` với bản Open WebUI không có MCP) sẽ trả lỗi upstream, không bị giả lập.

### Python OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(
    api_key="YOUR_PROXY_KEY",
    base_url="http://127.0.0.1:8080/v1",
    default_headers={"X-Website-ID": "site-a"},
    max_retries=0,  # tránh SDK tự phát lại request chat khi lỗi
)
model_id = client.models.list().data[0].id
print(client.chat.completions.create(
    model=model_id,
    messages=[{"role": "user", "content": "Xin chào!"}],
).choices[0].message.content)
```

Ứng dụng OpenAI-compatible: Base URL `http://127.0.0.1:8080/v1`, API key là khóa proxy, model là ID lấy ở trên. Dùng custom header để ghim website/tài khoản nếu ứng dụng hỗ trợ.

## 5. Điều phối, cooldown và lỗi

- Mặc định round-robin **trong từng website**; bỏ qua tài khoản đang cooldown và tài khoản/website bị tắt. Header `X-Website-ID` giới hạn pool theo website; `X-Account-ID` chọn đúng một tài khoản và **tắt failover**. Ghim cả hai thì acc phải thuộc website đó, nếu không trả `400`.
- Phản hồi có `X-Website-ID`/`X-Account-ID` cho biết nguồn thực tế. Hai header này chỉ định tuyến, **không phải xác thực riêng cho từng client**; mọi API `/v1/*` vẫn cần cùng khóa Bearer.
- Lỗi đăng nhập, lỗi mạng ở giai đoạn chuẩn bị/đăng nhập và upstream `5xx` trước khi chat thành công có thể chuyển sang tài khoản khác **trong cùng phạm vi đã chọn** (không bao giờ nhảy sang website khác khi đã ghim website).
- **Lỗi mạng của request chat đã gửi không được tự retry/failover**, vì upstream có thể đã bắt đầu sinh nội dung. Sau khi bắt đầu streaming cũng không failover. Cần cân nhắc điều này khi cấu hình retry ở client.
- Upstream `401`: đăng nhập lại và thử lại đúng một lần trên **cùng tài khoản**. `403`: không retry, không failover.
- Upstream `429`: tôn trọng `Retry-After`, đưa tài khoản vào cooldown và trả lỗi; **không tự thử tài khoản khác cho request đó**. Không dùng pool để vượt giới hạn áp dụng toàn dịch vụ upstream.
- `ACCOUNT_COOLDOWN_SECONDS` cấu hình cooldown mặc định; `Retry-After` hợp lệ từ upstream được tôn trọng. Request sau mới chọn trong những tài khoản đủ điều kiện.

## Cách hoạt động và tương thích

1. Đăng nhập khi cần qua `POST /api/v1/auths/signin`; token từng tài khoản chỉ giữ trong RAM và dùng lại.
2. Models chuyển tới `/api/models`; chat chuyển tới `/api/chat/completions` trên server của website đã chọn.
3. JSON/SSE chuyển tiếp, không chuyển đổi schema riêng của plugin. Tham số chat được chuyển tiếp; tool calling, ảnh, RAG phụ thuộc Open WebUI/model/provider.
4. Cần tài khoản đăng nhập email/mật khẩu; OAuth/SSO hoặc MFA tương tác không tự chuyển thành đăng nhập bằng mật khẩu.
5. Chỉ hỗ trợ Models và Chat Completions, chưa có Responses API, embeddings, audio, upload hoặc quản lý lịch sử chat. Proxy không mở thêm quyền model hay quota upstream.
6. Lưu cấu hình: mỗi lần lưu ghi file tạm cùng thư mục rồi `rename` nguyên tử, quyền `0600`; nếu ghi thất bại, cấu hình đang chạy giữ nguyên và API trả lỗi. Request/stream đang chạy tiếp tục dùng cấu hình cũ tới khi kết thúc.
7. WebUI/API quản trị không dùng cookie và từ chối request cross-origin; không có CORS mở. Đặt sau HTTPS khi truy cập từ ngoài.
8. **Chống dò khoá admin**: sai 5 lần liên tiếp từ cùng một IP → khoá 1 phút, tăng dần (×2) tới 15 phút, trả `429` + `Retry-After`. Đúng khoá thì reset. `X-Forwarded-For` chỉ được tin khi peer là loopback/private (sau reverse proxy), nên client công khai không thể xoay header để né.
9. Header bảo mật: `Content-Security-Policy` (script **và style** chỉ `'self'`, **không** `unsafe-inline`; `object-src 'none'`, `base-uri 'none'`, `frame-ancestors 'none'`), `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, và `Strict-Transport-Security` **khi request đến bằng TLS** (trực tiếp hoặc `X-Forwarded-Proto: https`).

Khởi động và `/healthz` không xác minh đăng nhập. Dùng **Kiểm tra** trong trang Accounts hoặc gọi `/v1/models` có ghim tài khoản để xác minh từng kết nối thật.

## 6. Proxy đầu ra cho từng website và tài khoản

Đây là đường **proxy → Open WebUI**, không phải địa chỉ API mà client gọi. Cả đăng nhập và chat dùng proxy của nguồn được chọn.

Ưu tiên cấu hình: `accounts[].proxy_url` → `websites[].proxy_url` → `OUTBOUND_PROXY` → `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` của môi trường.

```dotenv
# Proxy chung; username/password là tùy chọn
OUTBOUND_PROXY=http://proxyuser:proxypass@127.0.0.1:3128
```

```json
{
  "websites": [
    {"id":"site-a","name":"A","base_url":"https://a.example.com","proxy_url":"socks5h://127.0.0.1:1080"},
    {"id":"site-b","name":"B","base_url":"https://b.example.com"}
  ],
  "accounts": [
    {"id":"acc-1","website_id":"site-a","email":"one@example.com","password":"password-1"},
    {"id":"acc-2","website_id":"site-a","email":"two@example.com","password":"password-2","proxy_url":"direct"}
  ]
}
```

- Hỗ trợ `http://`, `https://`, `socks5://`, `socks5h://`; URL proxy cần cổng rõ ràng. HTTP proxy chuyển HTTPS qua CONNECT. SOCKS5 xử lý đích qua proxy.
- Đặt `direct` để bỏ qua cả proxy môi trường cho nguồn đó.
- Proxy đã chỉ định trực tiếp không bị `NO_PROXY` bỏ qua. TLS của Open WebUI vẫn được xác minh; proxy không tắt xác minh chứng chỉ.
- Encode ký tự đặc biệt trong user/password URL, ví dụ `@` → `%40`. URL proxy và mật khẩu không được trả về trong WebUI hay API danh sách tài khoản.
- Trong Docker, `127.0.0.1` là container, không phải host. Dùng địa chỉ proxy mà container truy cập được.
- OTA dùng HTTPS và proxy môi trường chuẩn của Go, **không dùng proxy của từng website/tài khoản**. Nếu OTA cần proxy, cấu hình `HTTPS_PROXY` trong môi trường khi chạy lệnh OTA.

## 7. OTA: cập nhật không cần build lại trên máy chạy

OTA được kích hoạt **bằng lệnh CLI**, không tự tải/chạy code nền và không mở endpoint cập nhật qua HTTP. Chạy binary đã build, không dùng `go run . ota update` vì lệnh đó trỏ tới binary tạm của Go.

### Trên máy đang chạy proxy

**Không cần cấu hình gì.** Binary đã nhúng sẵn:

- `OTA_MANIFEST_URL` mặc định → `https://github.com/6zev/openwebui-proxy-api/releases/latest/download/manifest.json` (tag mới tự nằm ở `latest`, không phải sửa URL mỗi bản)
- `OTA_PUBLIC_KEY` mặc định → `gdwbJrzsMFrboQ9Z8BZYLQtS/HiM0FYHNaIjjPu69WU=`, gắn cứng trong code bằng `ExpectedOTAKey`

```bash
./openwebui-proxy ota check
./openwebui-proxy ota update
```

Chỉ đặt `OTA_MANIFEST_URL`/`OTA_PUBLIC_KEY` khi tự vận hành server phát hành hoặc xoay khóa: biến môi trường luôn thắng giá trị nhúng. Xoay khóa thì phải đổi **cả** `ExpectedOTAKey` trong `ota.go` và secret `OTA_PRIVATE_KEY_B64`, nếu không update sẽ dừng vì sai chữ ký (fail closed).

Redirect chỉ được theo khi đích vẫn là HTTPS và thuộc `github.com`, `objects.githubusercontent.com`, `release-assets.githubusercontent.com`, tối đa 5 bước — GitHub dùng redirect này cho `/latest/download` và asset CDN.

```bash
./openwebui-proxy version
./openwebui-proxy check     # chỉ xem có bản mới không
./openwebui-proxy update    # tải, xác minh chữ ký, cài đặt
./openwebui-proxy rollback  # quay về binary trước
```

`update`/`check`/`rollback` là lệnh cấp cao; dạng tương đương `ota check|update|rollback` vẫn dùng được.
# Dừng tiến trình cũ và khởi động lại bằng cách bạn đã triển khai.
# Nếu đang dùng systemd với tên service tương ứng:
sudo systemctl restart openwebui-proxy

# Quay về bản trước nếu cần, sau đó restart service:
./openwebui-proxy ota rollback
sudo systemctl restart openwebui-proxy
```

`ota update` tải binary phù hợp `GOOS/GOARCH`, xác minh chữ ký manifest, SHA-256, kích thước và định dạng executable; ghi tệp tạm cùng filesystem rồi đổi tên nguyên tử. Binary cũ được lưu cạnh binary hiện tại với đuôi `.previous`; rollback khôi phục bản đó và giữ backup. Cần quyền ghi **thư mục chứa binary**. Khởi chạy lệnh từ thư mục có `.env` hoặc cung cấp biến môi trường tương ứng.

**Không tự restart tiến trình đang phục vụ.** Sau update/rollback, tiến trình cũ vẫn chạy phiên bản cũ tới khi được restart. Chưa có auto-health rollback hoặc lịch tự cập nhật. Nếu binary mới hỏng tới mức lệnh rollback không chạy được, khôi phục thủ công từ `.previous` rồi restart.

HTTPS bắt buộc cho manifest/chữ ký/binary; redirect bị chặn. Manifest có chữ ký được kiểm tra trên đúng bytes trước khi parse. Chỉ cài phiên bản mới hơn theo dạng `vX.Y.Z`, giới hạn tải 256 MiB. Public key phải lấy từ kênh tin cậy; private key chỉ giữ tại máy ký phát hành. Bản backup và thư mục binary cũng cần được bảo vệ khỏi chỉnh sửa ngoài ý muốn.

### Phát hành qua GitHub Actions

Workflow `.github/workflows/release.yml` chạy thủ công: kiểm tra version, `gofmt`/`go vet`/`go test -race`, build linux `amd64`+`arm64`, ký manifest bằng secret `OTA_PRIVATE_KEY_B64`, rồi tạo GitHub Release (không ghi đè release đã có).

```bash
gh workflow run release.yml --repo 6zev/openwebui-proxy-api -f version=v0.2.1
gh run watch --repo 6zev/openwebui-proxy-api
```

Asset gồm hai binary, `manifest.json` và `manifest.json.sig` — chính là các file `OTA_MANIFEST_URL` trỏ tới. `OTA_PRIVATE_KEY_B64` là base64 của file text private key do `ota keygen` tạo; không đưa khóa vào repo hay log.

### Tạo bản phát hành của riêng bạn

```bash
# Thực hiện tại máy phát hành, chỉ tạo khóa một lần:
mkdir -p keys
go build -o openwebui-proxy .
./openwebui-proxy ota keygen keys/ota-public.key keys/ota-private.key
# Dán nội dung keys/ota-public.key vào OTA_PUBLIC_KEY của máy chạy.

# Build Linux amd64 + arm64, tạo manifest và chữ ký:
sh scripts/release.sh v0.2.0 keys/ota-private.key https://updates.example.com/openwebui
```

Script yêu cầu Go cùng `sha256sum`; output ở `dist/v0.2.0/`. Upload **nội dung** thư mục đó lên URL đã truyền: hai binary, `manifest.json`, `manifest.json.sig`. Không chỉnh lại manifest sau khi ký. Cập nhật binary trước, sau đó đưa manifest và signature mới lên đồng bộ; nếu client tải giữa hai thời điểm không khớp chữ ký, lệnh sẽ dừng và giữ binary cũ.

Định dạng manifest:

```json
{
  "version": "v0.2.0",
  "assets": [
    {
      "os": "linux",
      "arch": "amd64",
      "url": "https://updates.example.com/openwebui/openwebui-proxy_v0.2.0_linux_amd64",
      "sha256": "REPLACE_WITH_64_HEX_CHARACTERS",
      "size": 12345678
    }
  ]
}
```

Chữ ký detached Ed25519 dạng base64 nằm tại `OTA_MANIFEST_URL` + `.sig`. `ota keygen` và `ota sign` luôn tạo file mới, không đè khóa/chữ ký đã có. Nếu build thủ công, gắn version thật bằng `go build -ldflags='-X main.Version=v0.2.0' -o openwebui-proxy .`; build mặc định mang version `v0.2.0`.

### Khi dùng Docker

Compose mặc định filesystem chỉ đọc: **không chạy OTA thay binary bên trong container**. Triển khai image mới bằng:

```bash
VERSION=v0.2.0 docker compose up -d --build --force-recreate
```

Lệnh trên build **mã nguồn hiện có**; lấy mã bản mới trước khi chạy. `VERSION` chỉ gắn nhãn phiên bản binary, không tự tải source. Với image đã publish, dùng pull/recreate theo image tag. OTA binary hiện hỗ trợ thay thế tại chỗ trên Unix; Windows dùng thay binary bên ngoài tiến trình.

## Kiểm thử

```bash
go test -race ./...
```

GitHub Actions hiện có: `ci` (gofmt/vet/race test trên mỗi push) và `release` (build + ký + tạo Release). Bản `v0.2.0` đã phát hành qua action; asset tải về đã xác minh SHA-256 khớp manifest, chữ ký Ed25519 hợp lệ và binary amd64 chạy in ra `v0.2.0`.

Đã chạy thành công **98 bài test (gồm test con)** bằng race detector, cùng `go vet ./...`: proxy nhiều website, round-robin và ghim nguồn, token riêng, HTTP proxy có xác thực, SOCKS5, hủy/đứt SSE, WebUI/API quản trị (CRUD, phân quyền, chống cross-origin, ẩn secrets, lưu nguyên tử, chạy nền khi ghi lỗi), dotenv và OTA có chữ ký.

WebUI cũng đã được kiểm thử trên **Chromium headless thật**: đăng nhập, tạo hai website, tạo/sửa tài khoản, giữ mật khẩu khi để trống, ẩn mật khẩu khỏi DOM, chặn xóa website còn tài khoản, `401` khi sai khóa, và cấu hình còn nguyên sau khi tải lại trang.

Bộ test vẫn dùng upstream giả lập. **Chưa kiểm thử thông tin đăng nhập hoặc kết nối với Open WebUI thật.** Compose đã cập nhật nhưng chưa build container tại môi trường này vì chưa có Docker CLI. Phiên bản Node.js trước đây được lưu tại `legacy/node/`.

## Tham chiếu upstream

- https://github.com/open-webui/open-webui/blob/main/backend/open_webui/routers/auths.py
- https://github.com/open-webui/open-webui/blob/main/backend/open_webui/main.py

Endpoint phụ thuộc phiên bản Open WebUI đang triển khai; một số plugin/phiên bản có thể cần adapter riêng để tương thích đầy đủ SDK OpenAI.

## Mẫu cấu hình

- [.env.example](.env.example)
- [websites.example.json](websites.example.json)
- [accounts.example.json](accounts.example.json)
