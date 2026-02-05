<div align="center">
  <img src="docs/media/logo.png" alt="telegram-executor logo" width="120" height="120" />
  <h1>telegram-executor</h1>
  <p>💬 Telegram async-executor для <code>yaml-mcp-server</code>: отправляет пользователю 2-5 вариантов выбора и принимает «свой вариант» текстом или голосом.</p>
</div>

![Go Version](https://img.shields.io/github/go-mod/go-version/codex-k8s/telegram-executor)
[![Go Reference](https://pkg.go.dev/badge/github.com/codex-k8s/telegram-executor.svg)](https://pkg.go.dev/github.com/codex-k8s/telegram-executor)

🇬🇧 English version: [README.md](README.md)

## Что делает сервис

`telegram-executor` получает async-запросы выполнения от `yaml-mcp-server`, отправляет сообщение в Telegram с вариантами, ждёт ответ пользователя и возвращает callback обратно в `yaml-mcp-server`.

Поддерживаемый сценарий:
- выбор одного из заранее заданных вариантов (2..5)
- кнопка `Свой вариант` и ввод текстом/голосом
- обработка таймаута с callback `status=error`

## Поток выполнения

1. `yaml-mcp-server` вызывает `POST /execute` и получает `202 Accepted`.
2. `telegram-executor` отправляет сообщение в Telegram с кнопками вариантов.
3. Пользователь выбирает вариант или отправляет свой ответ.
4. `telegram-executor` отправляет callback на URL из запроса.

## Установка

Требуется Go >= 1.25.5.

```bash
go install github.com/codex-k8s/telegram-executor/cmd/telegram-executor@latest
```

## Переменные окружения

Все переменные имеют префикс `TG_EXECUTOR_`:

- `TG_EXECUTOR_TOKEN` - токен Telegram-бота (обязательно)
- `TG_EXECUTOR_CHAT_ID` - разрешённый chat id (обязательно)
- `TG_EXECUTOR_HTTP_HOST` - host HTTP-сервера (обязательно)
- `TG_EXECUTOR_HTTP_PORT` - порт HTTP-сервера (по умолчанию `8080`)
- `TG_EXECUTOR_LANG` - язык сообщений (`en`/`ru`, по умолчанию `en`)
- `TG_EXECUTOR_EXECUTION_TIMEOUT` - общий таймаут ожидания (по умолчанию `1h`)
- `TG_EXECUTOR_TIMEOUT_MESSAGE` - текст при таймауте (опционально)
- `TG_EXECUTOR_WEBHOOK_URL` - URL для Telegram webhook режима (опционально)
- `TG_EXECUTOR_WEBHOOK_SECRET` - секрет для Telegram webhook режима (опционально)
- `TG_EXECUTOR_OPENAI_API_KEY` - ключ OpenAI для распознавания голоса (опционально)
- `TG_EXECUTOR_STT_MODEL` - модель STT (по умолчанию `gpt-4o-mini-transcribe`)
- `TG_EXECUTOR_STT_TIMEOUT` - таймаут STT (по умолчанию `30s`)
- `TG_EXECUTOR_LOG_LEVEL` - `debug|info|warn|error`
- `TG_EXECUTOR_SHUTDOWN_TIMEOUT` - таймаут graceful shutdown (по умолчанию `10s`)

Webhook-режим включается только если заданы оба параметра: `TG_EXECUTOR_WEBHOOK_URL` и `TG_EXECUTOR_WEBHOOK_SECRET`.

## API

### POST /execute

Пример запроса:

```json
{
  "correlation_id": "req-123",
  "tool": {
    "name": "telegram_request_feedback",
    "title": "Request user feedback"
  },
  "arguments": {
    "question": "Какой rollout для релиза выбрать?",
    "context": "Прод-выкатка billing-api",
    "options": [
      "Canary на 10% трафика",
      "Blue/green переключение",
      "Отложить релиз"
    ],
    "allow_custom": true
  },
  "spec": {
    "kind": "telegram_feedback_v1",
    "options_min": 2,
    "options_max": 5,
    "custom_option_label": "Свой вариант"
  },
  "lang": "ru",
  "markup": "markdown",
  "timeout_sec": 3600,
  "callback": {
    "url": "http://yaml-mcp-server.codex-system.svc.cluster.local/executors/webhook"
  }
}
```

Ответ:

```json
{
  "status": "pending",
  "result": "queued",
  "correlation_id": "req-123"
}
```

### Callback в yaml-mcp-server

Успешный выбор:

```json
{
  "correlation_id": "req-123",
  "status": "success",
  "result": {
    "question": "Какой rollout для релиза выбрать?",
    "selected_option": "Canary на 10% трафика",
    "selected_index": 0,
    "custom": false,
    "input_mode": "button"
  },
  "tool": "telegram_request_feedback"
}
```

Для своего варианта `custom=true`, `input_mode` будет `text` или `voice`.

Пример ошибки:

```json
{
  "correlation_id": "req-123",
  "status": "error",
  "result": "execution timeout",
  "tool": "telegram_request_feedback"
}
```

## Голосовой ввод

Если задан `TG_EXECUTOR_OPENAI_API_KEY`, голосовые сообщения распознаются через OpenAI.

Нужен `ffmpeg`:

```bash
sudo apt-get install -y ffmpeg
```

## Безопасность

- Сервис stateless.
- Решения принимаются только из одного chat id.
- Callback endpoint не защищён shared-secret по умолчанию, ограничивайте доступ сетью.

## Лицензия

См. [LICENSE](LICENSE).
