import asyncio
import os
import uuid
import re
import logging
from base64 import b64encode, b64decode

import telegram

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(levelname)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger(__name__)

BASE_URL: str = os.environ.get("BASE_URL", "https://api.telegram.org/bot")
BOT_TOKEN: str = os.environ.get("SERVER_BOT_TOKEN", "")

if not BOT_TOKEN:
    raise ValueError("SERVER_BOT_TOKEN environment variable is required")

connects = {}


async def run():
    async with telegram.Bot(base_url=BASE_URL, token=BOT_TOKEN) as bot:
        last_id = None

        async def connect(message: telegram.Message):
            if not message.text:
                return

            group = re.search(r"^CONNECT (\S+) (\S+) (\d+)$", message.text)
            if group is None:
                return

            request_id = group.group(1)
            host = group.group(2)
            port = int(group.group(3))

            if request_id in connects:
                logger.warning(f"Already connected with request_id {request_id}")
                return

            logger.info(f"Connecting to {host}:{port} with request_id {request_id}")
            stream_id = uuid.uuid4().hex
            read, write = await asyncio.open_connection(host, port)
            connects[stream_id] = (read, write)

            async def reader():
                while True:
                    data = await read.read(2000)
                    if not data:
                        logger.info(f"Connection closed for stream_id {stream_id}")
                        if stream_id in connects:
                            del connects[stream_id]
                            await bot.send_message(
                                chat_id=message.chat.id,
                                text=f"CLOSED {stream_id}",
                            )

                        break

                    logger.debug(f"Received data on stream_id {stream_id}: {data}")

                    await bot.send_message(
                        chat_id=message.chat.id,
                        text=f"RECV {stream_id} {b64encode(data).decode('utf-8')}",
                    )

            asyncio.create_task(reader())
            await bot.send_message(
                chat_id=message.chat.id, text=f"OK {request_id} {stream_id}"
            )

        async def send(message: telegram.Message):
            if not message.text:
                return

            group = re.search(r"^SEND (\S+) (\S+)$", message.text)
            if group is None:
                return

            stream_id = group.group(1)
            data = b64decode(group.group(2))

            if stream_id not in connects:
                logger.warning(f"No connection found for stream_id {stream_id}")
                return

            logger.debug(f"Sending data to stream_id {stream_id}: {data}")
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
                logger.warning(f"No connection found for stream_id {stream_id}")
                return

            logger.info(f"Closing connection for stream_id {stream_id}")
            _, write = connects[stream_id]
            write.close()
            del connects[stream_id]

            await bot.send_message(
                chat_id=message.chat.id,
                text=f"CLOSED {stream_id}",
            )

        while True:
            try:
                await asyncio.sleep(0.001)

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

                    await connect(update.message)
                    await send(update.message)
                    await close(update.message)
            except Exception as e:
                logger.error(f"An error occurred: {e}", exc_info=True)
                await asyncio.sleep(0.001)


if __name__ == "__main__":
    asyncio.run(run())