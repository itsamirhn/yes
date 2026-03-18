import asyncio
import base64
import json
import os
import struct
import uuid
import logging
import re

import telegram
from telegram.request import HTTPXRequest
from aiohttp import web

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(levelname)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger(__name__)

BASE_URL: str = os.environ.get("BASE_URL", "https://api.telegram.org/bot")
BOT_TOKEN: str = os.environ.get("CLIENT_BOT_TOKEN", "")
CHAT_ID: str | int = os.environ.get("CHAT_ID", "")
WEBHOOK_URL: str = os.environ.get("CLIENT_WEBHOOK_URL", "")
WEBHOOK_PORT: int = int(os.environ.get("CLIENT_WEBHOOK_PORT", "8443"))

if not BOT_TOKEN:
    raise ValueError("CLIENT_BOT_TOKEN environment variable is required")
if not CHAT_ID:
    raise ValueError("CHAT_ID environment variable is required")

ENABLE_FILES = os.environ.get("ENABLE_FILES", "").lower() in ("1", "true", "yes")

bot = telegram.Bot(
    base_url=BASE_URL,
    token=BOT_TOKEN,
    request=HTTPXRequest(connection_pool_size=32, pool_timeout=30.0, read_timeout=30.0, write_timeout=30.0, connect_timeout=30.0),
)
connects = []

LISTEN_HOST = os.environ.get("LISTEN_HOST", "0.0.0.0")
LISTEN_PORT = int(os.environ.get("LISTEN_PORT", "1080"))
CHUNK_SIZE = 3100 if not ENABLE_FILES else 4096
TEXT_MAX_RAW = 3200


async def tg_send(coro_func, *args, **kwargs):
    """Call a telegram bot method with retry on flood control."""
    while True:
        try:
            return await coro_func(*args, **kwargs)
        except telegram.error.RetryAfter as e:
            logger.warning(f"Flood control, retrying in {e.retry_after}s")
            await asyncio.sleep(e.retry_after)
        except telegram.error.TimedOut:
            logger.warning("Telegram timed out, retrying in 1s")
            await asyncio.sleep(1)


class AsyncBytesIO:
    def __init__(self):
        self._queue = asyncio.Queue()
        self._buffer = b""
        self._closed = False

    async def write(self, data: bytes):
        if self._closed:
            raise ValueError("I/O operation on closed file")
        await self._queue.put(data)

    async def read(self, size: int = -1):
        while not self._buffer and not self._closed:
            try:
                data = await asyncio.wait_for(self._queue.get(), timeout=60)
                if data is None:
                    self._closed = True
                    break
                self._buffer += data
            except asyncio.TimeoutError:
                continue

        while not self._queue.empty():
            try:
                data = self._queue.get_nowait()
                if data is None:
                    self._closed = True
                    break
                self._buffer += data
            except asyncio.QueueEmpty:
                break

        if size == -1:
            result = self._buffer
            self._buffer = b""
        else:
            result = self._buffer[:size]
            self._buffer = self._buffer[size:]

        return result

    async def close(self):
        if not self._closed:
            self._closed = True
            await self._queue.put(None)


def pack_frames(frames):
    buf = bytearray()
    for stream_id, data in frames:
        buf += bytes.fromhex(stream_id)
        buf += struct.pack(">I", len(data))
        buf += data
    return bytes(buf)


def unpack_frames(raw):
    frames = []
    offset = 0
    while offset < len(raw):
        if offset + 20 > len(raw):
            break
        stream_id = raw[offset:offset + 16].hex()
        length = struct.unpack(">I", raw[offset + 16:offset + 20])[0]
        offset += 20
        data = raw[offset:offset + length]
        offset += length
        frames.append((stream_id, data))
    return frames


class SendQueue:
    def __init__(self, bot, chat_id, prefix):
        self._bot = bot
        self._chat_id = chat_id
        self._prefix = prefix
        self._queue = asyncio.Queue()
        self._task = None

    def start(self):
        self._task = asyncio.create_task(self._flush_loop())

    async def push(self, stream_id, data):
        await self._queue.put((stream_id, data))

    async def _flush_loop(self):
        while True:
            first = await self._queue.get()
            frames = [first]
            while not self._queue.empty():
                try:
                    frames.append(self._queue.get_nowait())
                except asyncio.QueueEmpty:
                    break
            await self._send_batch(frames)

    async def _send_batch(self, frames):
        raw = pack_frames(frames)
        if len(raw) <= TEXT_MAX_RAW:
            encoded = base64.b85encode(raw).decode()
            await tg_send(
                self._bot.send_message,
                chat_id=self._chat_id,
                text=f"{self._prefix} {encoded}",
            )
        elif ENABLE_FILES:
            await tg_send(
                self._bot.send_document,
                chat_id=self._chat_id,
                document=raw,
                filename=f"{self._prefix}.bin",
            )
        else:
            for frame in frames:
                chunk_raw = pack_frames([frame])
                if len(chunk_raw) <= TEXT_MAX_RAW:
                    encoded = base64.b85encode(chunk_raw).decode()
                    await tg_send(
                        self._bot.send_message,
                        chat_id=self._chat_id,
                        text=f"{self._prefix} {encoded}",
                    )
                else:
                    logger.warning(f"Frame too large for text and files disabled, dropping {len(chunk_raw)} bytes")


send_queue = SendQueue(bot, CHAT_ID, "SEND")


async def open_connection():
    request_id = uuid.uuid4().hex
    await tg_send(bot.send_message, chat_id=CHAT_ID, text=f"CONNECT {request_id}")

    while not any(request_id == c[0] for c in connects):
        await asyncio.sleep(0.001)

    rb, stream_id = next((c[2], c[1]) for c in connects if c[0] == request_id)
    return rb, stream_id


async def handle_message(message: telegram.Message):
    if not message:
        return

    # Handle OK
    if message.text:
        group = re.search(r"^OK (\S+) (\S+)$", message.text)
        if group:
            request_id = group.group(1)
            stream_id = group.group(2)
            logger.info(f"OK for request {request_id}, stream {stream_id}")
            if not any(request_id == c[0] for c in connects):
                rb = AsyncBytesIO()
                connects.append((request_id, stream_id, rb))
            return

    # Handle RECV text (multiplexed frames)
    if message.text:
        group = re.search(r"^RECV (\S+)$", message.text)
        if group:
            encoded = group.group(1)
            raw = base64.b85decode(encoded)
            for stream_id, data in unpack_frames(raw):
                for c in connects:
                    if c[1] == stream_id:
                        await c[2].write(data)
                        break
            return

    # Handle RECV.bin file
    if ENABLE_FILES and message.document:
        filename = message.document.file_name
        if filename == "RECV.bin":
            file = await message.document.get_file()
            raw = bytes(await file.download_as_bytearray())
            for stream_id, data in unpack_frames(raw):
                for c in connects:
                    if c[1] == stream_id:
                        await c[2].write(data)
                        break
            return

    # Handle CLOSED
    if message.text:
        group = re.search(r"^CLOSED (\S+)$", message.text)
        if group:
            stream_id = group.group(1)
            logger.info(f"Stream {stream_id} closed by server")
            for c in connects:
                if c[1] == stream_id:
                    await c[2].close()
                    break
            connects[:] = [c for c in connects if c[1] != stream_id]
            return


async def run_polling():
    last_id = None
    while True:
        try:
            await asyncio.sleep(0.001)
            updates = await bot.get_updates(
                offset=last_id + 1 if last_id else None, limit=10
            )
            if not updates:
                continue
            last_id = max(u.update_id for u in updates)
            for update in updates:
                message = update.channel_post
                if message is None:
                    continue
                await handle_message(message)
        except Exception as e:
            logger.error(f"Bot error: {e}", exc_info=True)
            await asyncio.sleep(0.001)


async def run_webhook():
    app = web.Application()

    async def webhook_handler(request):
        data = await request.json()
        update = telegram.Update.de_json(data, bot)
        if update and update.channel_post:
            await handle_message(update.channel_post)
        return web.Response(text="ok")

    app.router.add_post(f"/{BOT_TOKEN}", webhook_handler)

    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "0.0.0.0", WEBHOOK_PORT)
    await site.start()
    logger.info(f"Webhook server listening on port {WEBHOOK_PORT}")

    # Set webhook with Telegram
    await tg_send(bot.set_webhook, url=f"{WEBHOOK_URL}/{BOT_TOKEN}")
    logger.info(f"Webhook set to {WEBHOOK_URL}/{BOT_TOKEN}")

    # Keep running
    while True:
        await asyncio.sleep(3600)


async def forward_to_writer(reader, writer, stream_id=None):
    try:
        while True:
            data = await reader.read(CHUNK_SIZE)
            if not data:
                break
            if stream_id is not None:
                await send_queue.push(stream_id, data)
            else:
                writer.write(data)
                await writer.drain()
    except Exception as e:
        logger.error(f"Forward error: {e}")
    finally:
        try:
            if stream_id is not None:
                await tg_send(bot.send_message, chat_id=CHAT_ID, text=f"CLOSE {stream_id}")
            else:
                writer.close()
        except Exception:
            pass


async def handle_client(client_reader, client_writer):
    try:
        server_reader, stream_id = await open_connection()

        client_to_server = asyncio.create_task(
            forward_to_writer(client_reader, None, stream_id=stream_id)
        )
        server_to_client = asyncio.create_task(
            forward_to_writer(server_reader, client_writer)
        )

        await asyncio.gather(client_to_server, server_to_client, return_exceptions=True)
    except Exception as e:
        logger.error(f"Client handler error: {e}")
    finally:
        try:
            client_writer.close()
        except Exception:
            pass


async def main():
    print(f"Starting tunnel client on {LISTEN_HOST}:{LISTEN_PORT}")
    print(f"File transfers: {'enabled' if ENABLE_FILES else 'disabled'}")
    print(f"Mode: {'webhook' if WEBHOOK_URL else 'polling'}")

    send_queue.start()

    if WEBHOOK_URL:
        bot_task = asyncio.create_task(run_webhook())
    else:
        bot_task = asyncio.create_task(run_polling())

    await asyncio.sleep(2)

    server = await asyncio.start_server(handle_client, LISTEN_HOST, LISTEN_PORT)
    print(f"Tunnel listening on {LISTEN_HOST}:{LISTEN_PORT}")

    async with server:
        await asyncio.gather(bot_task, server.serve_forever(), return_exceptions=True)


if __name__ == "__main__":
    asyncio.run(main())
