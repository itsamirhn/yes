import asyncio
import base64
import json
import os
import struct
import uuid
import re
import logging

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
BOT_TOKEN: str = os.environ.get("SERVER_BOT_TOKEN", "")
UPSTREAM_HOST: str = os.environ.get("UPSTREAM_HOST", "127.0.0.1")
UPSTREAM_PORT: int = int(os.environ.get("UPSTREAM_PORT", "1080"))
WEBHOOK_URL: str = os.environ.get("SERVER_WEBHOOK_URL", "")
WEBHOOK_PORT: int = int(os.environ.get("SERVER_WEBHOOK_PORT", "8443"))

if not BOT_TOKEN:
    raise ValueError("SERVER_BOT_TOKEN environment variable is required")

ENABLE_FILES = os.environ.get("ENABLE_FILES", "").lower() in ("1", "true", "yes")
CHUNK_SIZE = 3100 if not ENABLE_FILES else 4096
TEXT_MAX_RAW = 3200
connects = {}


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

    def set_chat_id(self, chat_id):
        self._chat_id = chat_id

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


async def run():
    async with telegram.Bot(
        base_url=BASE_URL,
        token=BOT_TOKEN,
        request=HTTPXRequest(connection_pool_size=32, pool_timeout=30.0, read_timeout=30.0, write_timeout=30.0, connect_timeout=30.0),
    ) as bot:
        last_id = None
        send_queue = SendQueue(bot, None, "RECV")

        async def handle_message(message: telegram.Message):
            if not message:
                return

            # Handle CONNECT
            if message.text:
                group = re.search(r"^CONNECT (\S+)$", message.text)
                if group:
                    request_id = group.group(1)
                    if request_id in connects:
                        return

                    logger.info(f"New tunnel request {request_id}, connecting to {UPSTREAM_HOST}:{UPSTREAM_PORT}")
                    stream_id = uuid.uuid4().hex

                    try:
                        read, write = await asyncio.open_connection(UPSTREAM_HOST, UPSTREAM_PORT)
                    except Exception as e:
                        logger.error(f"Failed to connect to upstream: {e}")
                        return

                    connects[stream_id] = (read, write)
                    send_queue.set_chat_id(message.chat.id)

                    async def reader(sid=stream_id, r=read, chat_id=message.chat.id):
                        while True:
                            data = await r.read(CHUNK_SIZE)
                            if not data:
                                logger.info(f"Upstream closed for stream {sid}")
                                if sid in connects:
                                    del connects[sid]
                                    await tg_send(bot.send_message, chat_id=chat_id, text=f"CLOSED {sid}")
                                break
                            await send_queue.push(sid, data)

                    asyncio.create_task(reader())
                    await tg_send(bot.send_message, chat_id=message.chat.id, text=f"OK {request_id} {stream_id}")
                    return

            # Handle SEND text (multiplexed frames)
            if message.text:
                group = re.search(r"^SEND (\S+)$", message.text)
                if group:
                    encoded = group.group(1)
                    raw = base64.b85decode(encoded)
                    for stream_id, data in unpack_frames(raw):
                        if stream_id not in connects:
                            continue
                        _, write = connects[stream_id]
                        write.write(data)
                        await write.drain()
                    return

            # Handle SEND.bin file
            if ENABLE_FILES and message.document:
                filename = message.document.file_name
                if filename == "SEND.bin":
                    file = await message.document.get_file()
                    raw = bytes(await file.download_as_bytearray())
                    for stream_id, data in unpack_frames(raw):
                        if stream_id not in connects:
                            continue
                        _, write = connects[stream_id]
                        write.write(data)
                        await write.drain()
                    return

            # Handle CLOSE
            if message.text:
                group = re.search(r"^CLOSE (\S+)$", message.text)
                if group:
                    stream_id = group.group(1)
                    if stream_id not in connects:
                        return
                    logger.info(f"Closing stream {stream_id}")
                    _, write = connects[stream_id]
                    write.close()
                    del connects[stream_id]
                    await tg_send(bot.send_message, chat_id=message.chat.id, text=f"CLOSED {stream_id}")
                    return

        async def run_polling():
            nonlocal last_id
            while True:
                try:
                    await asyncio.sleep(0.01)
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
                    logger.error(f"Error: {e}", exc_info=True)
                    await asyncio.sleep(0.01)

        async def run_webhook():
            app_web = web.Application()

            async def webhook_handler(request):
                data = await request.json()
                update = telegram.Update.de_json(data, bot)
                if update and update.channel_post:
                    await handle_message(update.channel_post)
                return web.Response(text="ok")

            app_web.router.add_post(f"/{BOT_TOKEN}", webhook_handler)

            runner = web.AppRunner(app_web)
            await runner.setup()
            site = web.TCPSite(runner, "0.0.0.0", WEBHOOK_PORT)
            await site.start()
            logger.info(f"Webhook server listening on port {WEBHOOK_PORT}")

            await tg_send(bot.set_webhook, url=f"{WEBHOOK_URL}/{BOT_TOKEN}")
            logger.info(f"Webhook set to {WEBHOOK_URL}/{BOT_TOKEN}")

            while True:
                await asyncio.sleep(3600)

        send_queue.start()
        print(f"File transfers: {'enabled' if ENABLE_FILES else 'disabled'}")
        print(f"Mode: {'webhook' if WEBHOOK_URL else 'polling'}")

        if WEBHOOK_URL:
            await run_webhook()
        else:
            await run_polling()


if __name__ == "__main__":
    asyncio.run(run())
