import asyncio
import base64
import os
import uuid
import re
import logging

import telegram

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

if not BOT_TOKEN:
    raise ValueError("SERVER_BOT_TOKEN environment variable is required")

CHUNK_SIZE = 4096
TEXT_MAX_RAW = 3200
connects = {}


async def run():
    async with telegram.Bot(base_url=BASE_URL, token=BOT_TOKEN) as bot:
        last_id = None

        async def connect(message: telegram.Message):
            if not message.text:
                return

            group = re.search(r"^CONNECT (\S+)$", message.text)
            if group is None:
                return

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

            async def reader():
                while True:
                    data = await read.read(CHUNK_SIZE)
                    if not data:
                        logger.info(f"Upstream closed for stream {stream_id}")
                        if stream_id in connects:
                            del connects[stream_id]
                            await bot.send_message(
                                chat_id=message.chat.id,
                                text=f"CLOSED {stream_id}",
                            )
                        break

                    if len(data) <= TEXT_MAX_RAW:
                        encoded = base64.b85encode(data).decode()
                        await bot.send_message(
                            chat_id=message.chat.id,
                            text=f"RECV {stream_id} {encoded}",
                        )
                    else:
                        await bot.send_document(
                            chat_id=message.chat.id,
                            document=data,
                            filename=f"RECV_{stream_id}.bin",
                        )

            asyncio.create_task(reader())
            await bot.send_message(
                chat_id=message.chat.id, text=f"OK {request_id} {stream_id}"
            )

        async def handle_send_text(message: telegram.Message):
            if not message.text:
                return

            group = re.search(r"^SEND (\S+) (\S+)$", message.text)
            if group is None:
                return

            stream_id = group.group(1)
            if stream_id not in connects:
                return

            data = base64.b85decode(group.group(2))
            _, write = connects[stream_id]
            write.write(data)
            await write.drain()

        async def send(message: telegram.Message):
            if not message.document:
                return

            filename = message.document.file_name
            if not filename or not filename.startswith("SEND_"):
                return

            stream_id = filename[5:-4]

            file = await message.document.get_file()
            data = bytes(await file.download_as_bytearray())

            if stream_id not in connects:
                return

            _, write = connects[stream_id]
            write.write(data)
            await write.drain()

        async def close(message: telegram.Message):
            if not message.text:
                return

            group = re.search(r"^CLOSE (\S+)$", message.text)
            if group is None:
                return

            stream_id = group.group(1)

            if stream_id not in connects:
                return

            logger.info(f"Closing stream {stream_id}")
            _, write = connects[stream_id]
            write.close()
            del connects[stream_id]

            await bot.send_message(
                chat_id=message.chat.id,
                text=f"CLOSED {stream_id}",
            )

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
                    await connect(message)
                    await handle_send_text(message)
                    await send(message)
                    await close(message)
            except Exception as e:
                logger.error(f"Error: {e}", exc_info=True)
                await asyncio.sleep(0.01)


if __name__ == "__main__":
    asyncio.run(run())
