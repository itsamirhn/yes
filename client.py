import asyncio
import os
import uuid
import logging
import re

import telegram

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(levelname)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger(__name__)

BASE_URL: str = os.environ.get("BASE_URL", "https://api.telegram.org/bot")
BOT_TOKEN: str = os.environ.get("CLIENT_BOT_TOKEN", "")
CHAT_ID: str | int = os.environ.get("CHAT_ID", "")

if not BOT_TOKEN:
    raise ValueError("CLIENT_BOT_TOKEN environment variable is required")
if not CHAT_ID:
    raise ValueError("CHAT_ID environment variable is required")

bot = telegram.Bot(base_url=BASE_URL, token=BOT_TOKEN)
connects = []

LISTEN_HOST = os.environ.get("LISTEN_HOST", "0.0.0.0")
LISTEN_PORT = int(os.environ.get("LISTEN_PORT", "1080"))
CHUNK_SIZE = 4096


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
        while len(self._buffer) < size or size == -1:
            try:
                data = await asyncio.wait_for(self._queue.get(), timeout=3)
                if data is None:
                    self._closed = True
                    break
                self._buffer += data
            except asyncio.TimeoutError:
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


async def open_connection():
    request_id = uuid.uuid4().hex
    await bot.send_message(chat_id=CHAT_ID, text=f"CONNECT {request_id}")

    while not any(request_id == c[0] for c in connects):
        await asyncio.sleep(0.001)

    rb, wb = next((c[2], c[3]) for c in connects if c[0] == request_id)
    return rb, wb


async def handle_ok(message: telegram.Message):
    if not message.text:
        return

    group = re.search(r"^OK (\S+) (\S+)$", message.text)
    if group is None:
        return

    request_id = group.group(1)
    stream_id = group.group(2)
    logger.info(f"OK for request {request_id}, stream {stream_id}")

    if any(request_id == c[0] for c in connects):
        return

    rb = AsyncBytesIO()

    class AsyncWriteBuffer:
        def __init__(self):
            self._buffer = b""

        async def write(self, data):
            self._buffer += data
            if len(self._buffer) >= CHUNK_SIZE:
                await self.flush()

        async def flush(self):
            if self._buffer:
                await bot.send_document(
                    chat_id=CHAT_ID,
                    document=self._buffer,
                    filename=f"SEND_{stream_id}.bin",
                )
                self._buffer = b""

        async def close(self):
            await self.flush()
            await bot.send_message(chat_id=CHAT_ID, text=f"CLOSE {stream_id}")

    wb = AsyncWriteBuffer()
    connects.append((request_id, stream_id, rb, wb))


async def handle_recv(message: telegram.Message):
    if not message.document:
        return

    filename = message.document.file_name
    if not filename or not filename.startswith("RECV_"):
        return

    stream_id = filename[5:-4]

    file = await message.document.get_file()
    data = bytes(await file.download_as_bytearray())

    for c in connects:
        if c[1] == stream_id:
            await c[2].write(data)
            return


async def handle_close(message: telegram.Message):
    if not message.text:
        return

    group = re.search(r"^CLOSED (\S+)$", message.text)
    if group is None:
        return

    stream_id = group.group(1)
    logger.info(f"Stream {stream_id} closed by server")

    for c in connects:
        if c[1] == stream_id:
            await c[2].close()
            break

    connects[:] = [c for c in connects if c[1] != stream_id]


async def run_bot():
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
                await handle_ok(message)
                await handle_recv(message)
                await handle_close(message)

        except Exception as e:
            logger.error(f"Bot error: {e}", exc_info=True)
            await asyncio.sleep(0.001)


async def forward_to_writer(reader, writer):
    """Forward from asyncio.StreamReader or AsyncBytesIO to AsyncWriteBuffer or StreamWriter"""
    try:
        while True:
            data = await reader.read(CHUNK_SIZE)
            if not data:
                break
            if hasattr(writer, "_buffer"):
                await writer.write(data)
                await writer.flush()
            else:
                writer.write(data)
                await writer.drain()
    except Exception as e:
        logger.error(f"Forward error: {e}")
    finally:
        try:
            if hasattr(writer, "_buffer"):
                await writer.close()
            else:
                writer.close()
        except Exception:
            pass


async def handle_client(client_reader, client_writer):
    """Accept a TCP connection and tunnel it through Telegram"""
    try:
        server_reader, server_writer = await open_connection()

        client_to_server = asyncio.create_task(
            forward_to_writer(client_reader, server_writer)
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

    bot_task = asyncio.create_task(run_bot())
    await asyncio.sleep(2)

    server = await asyncio.start_server(handle_client, LISTEN_HOST, LISTEN_PORT)
    print(f"Tunnel listening on {LISTEN_HOST}:{LISTEN_PORT}")

    async with server:
        await asyncio.gather(bot_task, server.serve_forever(), return_exceptions=True)


if __name__ == "__main__":
    asyncio.run(main())
