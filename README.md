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
openssl rand -hex 32   # PROXY_API_KEY
openssl rand -hex 32   # ADMIN_API_KEY (nên khác khóa proxy)
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

- `websites[]` mỗi phần tử là một Open WebUI độc lập: `id`, `name`, `base_url`, `proxy_url` tùy chọn, `disabled` tùy chọn.
- `accounts[]` gắn với một website qua `website_id`; `proxy_url` tùy chọn ghi đè proxy của website. `base_url` chỉ dùng cho tài khoản legacy không có `website_id`.
- ID (website và tài khoản) là duy nhất toàn cục, `1–64` ký tự ASCII: chữ, số, `.`, `_`, `-`. Không sửa ID sau khi tạo.
- URL có thể chứa tiền tố đường dẫn như `/webui`. Giữ xác minh TLS; dùng HTTPS khi upstream ở máy khác.
- Chạy trực tiếp sẽ tự đọc `.env` trong thư mục hiện tại; biến môi trường của tiến trình **ưu tiên hơn** `.env`. Parser hỗ trợ `KEY=value`, dấu nháy đơn/đôi và dòng comment bắt đầu bằng `#`; không nội suy `$VARIABLE`. Compose có quy tắc nội suy riêng: đặt giá trị nhạy cảm trong dấu nháy đơn nếu có `$`.
- Thay đổi qua WebUI có hiệu lực ngay cho request tiếp theo; không cần khởi động lại.
- Mô hình cũ vẫn dùng được: `ACCOUNTS_FILE` (mảng tài khoản, có thể có `base_url`) hoặc `OPENWEBUI_EMAIL`/`OPENWEBUI_PASSWORD`; cả hai chỉ được đọc khi **không** đặt `CONFIG_FILE`.
- `DEFAULT_MODEL` tùy chọn, là model ID dùng khi request không chỉ định model. Mặc định host `127.0.0.1`, port `8080`, timeout `300000` ms, cooldown `60` giây.

Đừng đưa `.env` hoặc `data/` lên Git hay gửi mật khẩu trong cuộc trò chuyện. Các file này đã được loại khỏi Git và Docker build context.

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
./openwebui-proxy ota check
./openwebui-proxy ota update
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

Đã chạy thành công **93 bài test (gồm test con)** bằng race detector, cùng `go vet ./...`: proxy nhiều website, round-robin và ghim nguồn, token riêng, HTTP proxy có xác thực, SOCKS5, hủy/đứt SSE, WebUI/API quản trị (CRUD, phân quyền, chống cross-origin, ẩn secrets, lưu nguyên tử, chạy nền khi ghi lỗi), dotenv và OTA có chữ ký.

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
