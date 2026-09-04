# x-tester

فرانت‌اند Go روی هسته **Xray**: لینک ساب را می‌گیرد، نودها را probe می‌کند، سالم‌ها را با API به هسته اصلی mount می‌کند و ترافیک ورودی را با sticky balancer از آن‌ها رد می‌کند.

## نیازمندی‌ها

- Go 1.25+
- باینری `xray` در `PATH` یا مسیر در کانفیگ (`xray_bin`)
- هدف اصلی اجرا: **Linux** (`direct`/dokodemo با `SO_ORIGINAL_DST` فقط روی Linux)

## ساخت و اجرا

```bash
go build -o bin/x-tester ./cmd/x-tester/
cp config.example.yaml config.yaml
# sub_urls و password و filter_country را تنظیم کن
./bin/x-tester -c config.yaml
```

خروج از TUI: `q` یا `Ctrl+C`.

کراس‌بیلد Linux:

```bash
./scripts/build-linux.sh                 # amd64 static
GOARCH=arm64 ./scripts/build-linux.sh    # arm64 static
```

### Release (GitHub Actions)

ورکفلو `.github/workflows/release.yml` بعد از پوش باینری می‌سازد و روی GitHub Release می‌گذارد:

| تریگر | خروجی |
|--------|--------|
| پوش به `main` / `master` | ریلیز rolling با تگ `latest` (prerelease) |
| پوش تگ `v*` | ریلیز نسخه‌دار (مثلاً `v1.0.0`) |
| Run workflow دستی | اختیاری با نام تگ دلخواه |

هدف‌ها: `linux-amd64` · `linux-arm64` · `darwin-amd64` · `darwin-arm64` (+ `.sha256` / `SHA256SUMS`)

```bash
git push origin main          # → Release "latest"
git tag v1.0.0 && git push origin v1.0.0   # → Release v1.0.0
```

## معماری خلاصه

1. **Xray main** — outboundهای سالم با `xray api ado/adi` اضافه/حذف می‌شوند  
2. **Xray probe** — instance جدا برای تست هر نود  
3. **Go listen** — socks5 / http / direct؛ auth فقط password (یوزرنیم آزاد برای hash)  
4. **Sticky balancer** — `hash_username` | `hash_ip` | `in_port`؛ fail→جایگزین ثابت؛ برگشت نود اصلی→برگشت sticky؛ وزن با latency  
5. **TUI ثابت** — alt-screen، بدون اسکرول ترمینال  
6. **SQLite** — وضعیت تست و ignoreها بین ری‌استارت‌ها  

پروتکل‌های ساب: `vmess://` `vless://` `trojan://` `ss://` (لیست یا base64).

## کانفیگ

نمونه کامل: [`config.example.yaml`](config.example.yaml)

| کلید | معنی |
|------|------|
| `xray_bin` | مسیر/نام باینری Xray |
| `sub_urls` | لینک‌های ساب |
| `sub_refresh` | فاصلهٔ refresh ساب |
| `http_check` | چک سلامت (URL، status، timeout، headers اختیاری) |
| `database` | مسیر SQLite (نسبت به پوشهٔ فایل کانفیگ یا مطلق) |
| `grouping` | تشخیص کشور (`ifconfig.io/country_code`) |
| `filter_country` | فقط این کد کشورها active؛ بقیه ignore و دیگر تست نمی‌شوند |
| `balancer` | استراتژی sticky |
| `listen` | mode / password / host / port یا range / unix |
| `probe.interval_active` | فاصلهٔ چک مجدد سالم‌ها |
| `probe.interval_failed` | فاصلهٔ چک مجدد failها |
| `probe.mount_batch` | هر N سالم → فوری mount روی main (`0` = فقط آخر راند) |
| `probe.max_active` | سقف نود active (`0` = نامحدود)؛ پر شد → فقط recheck؛ کم شد → دوباره fill |
| `probe.standby` | استخر گرم (تست‌شده، هنوز mount نشده)؛ با افت active فوری promote می‌شود |
| `probe.latency_tolerance` | اگر latency اکتیو از بهترین + این مقدار بیشتر باشد، حذف (نه standby)؛ با پینگ بهتر برمی‌گردد |
| `probe.concurrency` / `delay` | موازی‌سازی و فاصله بین تست‌ها |
| `stats` | HTTP JSON (`/stats`) |

### Auth ورودی

- فقط **password** از کانفیگ چک می‌شود  
- **username** هر چیزی باشد قبول است و برای `hash_username` استفاده می‌شود  

### بعد از ری‌استارت (DB)

- نودهای `ignored` دوباره تست نمی‌شوند  
- نودهایی که اخیراً چک شده‌اند تا اتمام `interval_*` رد می‌شوند  
- نودهای قبلیِ `active` بدون تست دوباره remount می‌شوند  

## Stats

```text
GET http://127.0.0.1:9090/stats
```

JSON شامل `active` / `failed` / `ignored` / `countries` / لیست نودها.

## ساختار پروژه

```text
cmd/x-tester/          ورودی
internal/app/          orchestration
internal/config/       YAML
internal/sub/          fetch + parse ساب
internal/xray/         process + API mount
internal/probe/        HTTP/geo check
internal/balancer/     sticky
internal/listen/       socks5/http/direct
internal/store/        SQLite
internal/tui/          داشبورد ترمینال
internal/stats/        HTTP JSON
internal/metrics/      ترافیک لحظه‌ای
internal/activity/     وضعیت کار جاری برای TUI
```

## عیب‌یابی

- خطای `add outbound` → پیام کامل در لاگ TUI؛ جزئیات آخرین شکست در workdir موقت: `last-api-fail.json`  
- لاگ هسته‌ها: `<tmpdir>/probe/probe.log` و `main/main.log`  
- دایرکتوری موقت معمولاً زیر `/tmp` یا `/var/folders/.../x-tester-*` است  

## لایسنس

پروژه خصوصی/شخصی مگر خلاف آن اعلام شود.
