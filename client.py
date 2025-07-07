import asyncio
import base64
import os
import uuid
import logging

import re
from base64 import b64encode, b64decode

import telegram

logging.basicConfig(
    level=logging.ERROR,
    format="%(asctime)s - %(levelname)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger(__name__)

BASE_URL = os.environ.get("BASE_URL", "https://api.telegram.org/bot")
BOT_TOKEN = os.environ.get("CLIENT_BOT_TOKEN")
CHAT_ID = os.environ.get("CHAT_ID")

connects = []

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
        if self._closed:
            return b""

        while len(self._buffer) < size or size == -1:
            try:
                data = await asyncio.wait_for(self._queue.get(), timeout=5)
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

    def close(self):
        self._closed = True


async def run():
    async with telegram.Bot(base_url=BASE_URL, token=BOT_TOKEN) as bot:
        last_id = None

        async def open_connection(host: str, port: int):
            request_id = uuid.uuid4().hex
            await bot.send_message(
                chat_id=CHAT_ID, text=f"CONNECT {request_id} {host} {port}"
            )

            while not any(request_id == connect[0] for connect in connects):
                await asyncio.sleep(1)

            rb, wb = next(
                (connect[2], connect[3])
                for connect in connects
                if connect[0] == request_id
            )

            return rb, wb

        async def loop():
            rb, wb = await open_connection("httpbin.org", 80)

            await wb.write(b"GET / HTTP/1.1\r\nHost: httpbin.org'\r\n\r\n")
            await wb.flush()
            data = b""
            while True:
                chunk = await rb.read(4096)
                if not chunk:
                    break
                data += chunk
            print("Received data:")
            print(data.decode("utf-8"))

            while True:
                await asyncio.sleep(1)

        async def ok(message: telegram.Message):
            group = re.search(r"^OK ([^\s]+) ([^\s]+)$", message.text)
            if group is None:
                return

            logger.info(f"Received OK message: {message.text}")

            request_id = group.group(1)
            stream_id = group.group(2)

            if any(request_id == connect[0] for connect in connects):
                logger.warning(f"Already connected with request_id {request_id}")
                return

            rb = AsyncBytesIO()

            class AsyncWriteBuffer:
                def __init__(self):
                    self._buffer = b""
                    self._buffer_size = 4096

                async def write(self, data):
                    self._buffer += data
                    # Auto-flush if buffer gets too large
                    if len(self._buffer) >= self._buffer_size:
                        await self.flush()

                async def flush(self):
                    if self._buffer:
                        await bot.send_message(
                            chat_id=CHAT_ID,
                            text=f"SEND {stream_id} {b64encode(self._buffer).decode('utf-8')}",
                        )
                        self._buffer = b""

            wb = AsyncWriteBuffer()
            connects.append((request_id, stream_id, rb, wb))

        async def recv(message: telegram.Message):
            group = re.search(r"^RECV ([^\s]+) (.+)$", message.text)
            if group is None:
                return

            stream_id = group.group(1)
            data = b64decode(group.group(2))

            if not any(connect[1] == stream_id for connect in connects):
                logger.warning(f"No connection found for stream_id {stream_id}")
                return

            rb = next(
                connect[2]
                for connect in connects
                if connect[1] == stream_id
            )
            await rb.write(data)

        async def close(message: telegram.Message):
            group = re.search(r"^CLOSED (\w+)$", message.text)
            if group is None:
                return

            request_id = group.group(1)
            logger.info(f"Received close message: {message.text}")
            connects[:] = [connect for connect in connects if connect[0] != request_id]

        asyncio.create_task(loop())

        while True:
            try:
                await asyncio.sleep(1)

                updates = await bot.get_updates(
                    offset=last_id + 1 if last_id else None,
                    limit=10,
                )

                if not updates:
                    continue

                last_id = max(update.update_id for update in updates)

                for update in updates:
                    if update.message and update.message.text:
                        logger.debug(f"Received message: {update.message.text}")

                    if update.message is None:
                        continue

                    # if update.message.date < datetime.utcnow() - timedelta(minutes=5):
                    #    continue

                    await ok(update.message)
                    await recv(update.message)
                    await close(update.message)

            except Exception as e:
                logger.error(f"An error occurred: {e}", exc_info=True)
                await asyncio.sleep(5)


if __name__ == "__main__":
    asyncio.run(run())
