"""Loopback-only HTTP and WebSocket forwarding for the native T3 client."""

import asyncio
from contextlib import suppress

import aiohttp
from aiohttp import web
from multidict import CIMultiDict
from yarl import URL

HOP_HEADERS = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "te", "trailers", "transfer-encoding", "upgrade",
}
PRIVATE_HEADERS = {"host", "x-agent37-key", "forwarded"}


def headers(source, *, request=False):
    excluded = HOP_HEADERS | PRIVATE_HEADERS
    for value in source.getall("Connection", []):
        excluded |= {part.strip().lower() for part in value.split(",")}
    return CIMultiDict(
        (key, value) for key, value in source.items()
        if key.lower() not in excluded
        and (not request or not key.lower().startswith(("sec-websocket-", "x-forwarded-")))
    )


async def pipe(source, destination):
    async for message in source:
        if message.type == aiohttp.WSMsgType.TEXT:
            await destination.send_str(message.data)
        elif message.type == aiohttp.WSMsgType.BINARY:
            await destination.send_bytes(message.data)
        elif message.type == aiohttp.WSMsgType.ERROR:
            break
    code = source.close_code
    await destination.close(code=code if code and code not in {1005, 1006, 1015} else 1011)


def application(origin, key, port):
    app = web.Application(client_max_size=64 * 1024 * 1024)
    local_host = f"127.0.0.1:{port}"

    async def connect_http(app):
        async with aiohttp.ClientSession(
            auto_decompress=False, cookie_jar=aiohttp.DummyCookieJar(),
            timeout=aiohttp.ClientTimeout(total=None, connect=30, sock_read=90),
        ) as client:
            app[client_key] = client
            yield

    async def forward(request):
        if request.host != local_host:
            raise web.HTTPForbidden(text="Use the loopback relay address")
        if request.headers.get("Origin") not in {None, f"http://{local_host}", "t3code://app"}:
            raise web.HTTPForbidden(text="Untrusted relay origin")
        address = URL(origin + request.rel_url.raw_path_qs, encoded=True)
        outgoing = headers(request.headers, request=True)
        outgoing["X-Agent37-Key"] = key
        client = request.app[client_key]
        try:
            if request.headers.get("Upgrade", "").lower() == "websocket":
                return await websocket(request, client, address, outgoing)
            payload = request.content if request.can_read_body else None
            async with client.request(
                request.method, address, headers=outgoing, data=payload, allow_redirects=False,
            ) as upstream:
                response = web.StreamResponse(status=upstream.status, headers=headers(upstream.headers))
                await response.prepare(request)
                async for chunk in upstream.content.iter_chunked(64 * 1024):
                    await response.write(chunk)
                return response
        except (aiohttp.ClientError, TimeoutError):
            raise web.HTTPBadGateway(text="Agent37 T3 endpoint is unavailable") from None

    client_key = web.AppKey("http_client", aiohttp.ClientSession)
    app.cleanup_ctx.append(connect_http)
    app.router.add_route("*", "/{path:.*}", forward)
    return app


async def websocket(request, client, address, outgoing):
    protocols = tuple(p.strip() for p in request.headers.get("Sec-WebSocket-Protocol", "").split(",") if p.strip())
    async with client.ws_connect(address, headers=outgoing, protocols=protocols, max_msg_size=64 * 1024 * 1024) as upstream:
        downstream = web.WebSocketResponse(
            protocols=(upstream.protocol,) if upstream.protocol else (), max_msg_size=64 * 1024 * 1024,
        )
        await downstream.prepare(request)
        tasks = [asyncio.create_task(pipe(upstream, downstream)), asyncio.create_task(pipe(downstream, upstream))]
        try:
            await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        finally:
            for task in tasks:
                task.cancel()
            with suppress(asyncio.CancelledError):
                await asyncio.gather(*tasks, return_exceptions=True)
        return downstream
