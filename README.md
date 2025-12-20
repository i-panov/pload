# pload

Многопоточный загрузчик файлов с поддержкой возобновления загрузки

## Особенности

- Многопоточная загрузка с автоматическим определением количества потоков
- Поддержка возобновления прерванной загрузки
- Проверка доступного места на диске перед загрузкой
- Поддержка HTTP/HTTPS протоколов
- Сохранение состояния загрузки для надежного возобновления
- Обработка ошибок и автоматические повторные попытки при сбоях
- Отображение прогресса загрузки
- Поддержка различных платформ (Linux, Windows, macOS)

## Установка

```bash
go build -o bin/pload
```

Или можно установить как CLI-инструмент:

```bash
go install github.com/i-panov/pload@latest
```

## Использование

```bash
# Простая загрузка
pload https://example.com/largefile.zip

# Загрузка с указанием выходного файла
pload -o myfile.zip https://example.com/largefile.zip

# Загрузка с 8 потоками
pload -w 8 https://example.com/largefile.zip

# Продолжение прерванной загрузки
pload --resume https://example.com/largefile.zip

# Подробный режим (debug)
pload -v https://example.com/largefile.zip

# Загрузка с игнорированием ошибок TLS
pload --insecure https://self-signed.badssl.com/file.zip
```

## Флаги

```
  -a, --user-agent string     User-Agent (по умолчанию "Go-MultiThread-Downloader/3.3.0")
  -f, --force                 Перезаписать без подтверждения
  -h, --help                  Показать помощь
  -k, --insecure              Игнорировать ошибки TLS сертификатов
  -t, --timeout duration      Таймаут HTTP-запроса (по умолчанию 1m0s)
  -u, --url string            URL файла (обязательно)
  -v, --verbose               Подробный вывод (эквивалент --log-level=debug)
  -w, --workers int           Количество потоков (0 = авто) (по умолчанию 0)
      --buffer-size int       Размер буфера (байт, max 8MiB) (по умолчанию 262144)
      --log-level string      Уровень логов: error,warn,info,debug (по умолчанию "info")
      --no-emoji              Отключить эмодзи в логах
      --retries int           Количество повторов на часть (по умолчанию 5)
      --retry-delay duration  Базовая задержка повтора (по умолчанию 2s)
  -o, --output string         Путь к выходному файлу (по умолчанию — имя из URL)
      --resume                Продолжить скачивание существующего файла (если возможно)
      --save-state            Сохранять состояние частей для надёжного resume (включено по умолчанию)
```

## Примеры

```bash
# Загрузка большого файла с 8 потоками и подробным выводом
pload -w 8 -v https://speed.hetzner.de/10GB.bin

# Продолжение загрузки с сохранением состояния
pload --resume --save-state https://example.com/largefile.zip

# Загрузка с кастомным User-Agent
pload -a "MyCustomAgent/1.0" https://example.com/file.zip

# Загрузка с увеличенным количеством повторов
pload --retries 10 --retry-delay 5s https://unreliable-server.com/file.zip
```

## Как это работает

1. Загрузчик сначала получает информацию о файле с помощью HEAD-запроса
2. Определяет поддержку многопоточной загрузки (Range-запросы)
3. Выделяет место на диске для файла
4. Разбивает файл на части и загружает их параллельно
5. Сохраняет состояние загрузки для возможности возобновления
6. Проверяет целостность файла после загрузки

## Лицензия

MIT License