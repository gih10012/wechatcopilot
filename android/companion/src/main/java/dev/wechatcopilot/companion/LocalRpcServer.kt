package dev.wechatcopilot.companion

import android.content.Context
import android.util.Log
import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject
import java.io.BufferedInputStream
import java.io.BufferedOutputStream
import java.io.EOFException
import java.net.InetAddress
import java.net.ServerSocket
import java.net.Socket
import java.net.URLDecoder
import java.nio.charset.StandardCharsets
import java.util.Locale
import java.util.concurrent.Executors

internal class LocalRpcServer(
    private val context: Context,
    private val port: Int,
) {
    private val socket = ServerSocket(port, 16, InetAddress.getByName("127.0.0.1"))
    private val workers = Executors.newFixedThreadPool(4) { runnable ->
        Thread(runnable, "wcc-companion-rpc").apply { isDaemon = true }
    }

    fun start() {
        Thread({ acceptLoop() }, "wcc-companion-listener").apply {
            isDaemon = true
            start()
        }
    }

    private fun acceptLoop() {
        try {
            while (!Thread.currentThread().isInterrupted) {
                val client = socket.accept()
                workers.execute { handle(client) }
            }
        } catch (error: Exception) {
            Log.e("WCCCompanion", "Loopback RPC listener stopped", error)
        }
    }

    private fun handle(socket: Socket) {
        socket.use {
            it.soTimeout = 10_000
            val output = BufferedOutputStream(it.getOutputStream())
            try {
                val request = HttpRequestParser.parse(BufferedInputStream(it.getInputStream()))
                val auth = request.headers["authorization"].orEmpty()
                val token = auth.removePrefix("Bearer ")
                if (!auth.startsWith("Bearer ") || !CompanionRuntime.tokenMatches(context, token)) {
                    writeJson(output, 401, JSONObject().put("error", "unauthorized"))
                    return
                }
                route(request, output)
            } catch (error: RequestException) {
                writeJson(output, error.status, JSONObject().put("error", error.message))
            } catch (error: JSONException) {
                writeJson(output, 400, JSONObject().put("error", "invalid JSON body"))
            } catch (error: Exception) {
                Log.w("WCCCompanion", "RPC request failed", error)
                writeJson(output, 500, JSONObject().put("error", "local companion error"))
            }
        }
    }

    private fun route(request: ParsedHttpRequest, output: BufferedOutputStream) {
        when {
            request.method == "GET" && request.path == "/v1/health" -> {
                writeJson(
                    output,
                    200,
                    JSONObject()
                        .put("ok", true)
                        .put("service", "wechatcopilot-wecom-companion")
                        .put("sequence", CompanionRuntime.currentSequence()),
                )
            }
            request.method == "GET" && request.path == "/v1/snapshot" -> {
                writeJson(output, 200, CompanionRuntime.snapshot().toJson())
            }
            request.method == "GET" && request.path == "/v1/events" -> {
                val after = request.query["after"]?.toLongOrNull()?.coerceAtLeast(0) ?: 0
                val limit = request.query["limit"]?.toIntOrNull()?.coerceIn(1, 500) ?: 100
                val events = CompanionRuntime.eventsAfter(after, limit)
                val encoded = JSONArray()
                events.forEach { encoded.put(it.toJson()) }
                val oldest = CompanionRuntime.oldestEventSequence()
                val complete = oldest == null || after >= oldest - 1
                writeJson(
                    output,
                    200,
                    JSONObject()
                        .put("events", encoded)
                        .put("next_cursor", events.lastOrNull()?.sequence ?: after)
                        .put("complete", complete),
                )
            }
            request.method == "POST" && request.path == "/v1/actions" -> {
                val json = JSONObject(request.body.toString(StandardCharsets.UTF_8))
                val action = CompanionAction(
                    kind = json.optString("kind"),
                    nodeId = json.optString("node_id"),
                    text = json.optString("text"),
					expectedSequence = json.optLong("expected_sequence", 0),
                )
                validateLocalAction(action)
                writeJson(output, 200, CompanionRuntime.perform(action).toJson())
            }
            else -> writeJson(output, 404, JSONObject().put("error", "not found"))
        }
    }

    private fun writeJson(output: BufferedOutputStream, status: Int, body: JSONObject) {
        val bytes = body.toString().toByteArray(StandardCharsets.UTF_8)
        val reason = when (status) {
            200 -> "OK"
            400 -> "Bad Request"
            401 -> "Unauthorized"
            404 -> "Not Found"
            503 -> "Service Unavailable"
            else -> "Internal Server Error"
        }
        val headers = buildString {
            append("HTTP/1.1 $status $reason\r\n")
            append("Content-Type: application/json; charset=utf-8\r\n")
            append("Content-Length: ${bytes.size}\r\n")
            append("Cache-Control: no-store\r\n")
            append("Connection: close\r\n\r\n")
        }.toByteArray(StandardCharsets.US_ASCII)
        output.write(headers)
        output.write(bytes)
        output.flush()
    }
}

internal fun validateLocalAction(action: CompanionAction) {
    when (action.kind) {
        "click", "check", "scroll_forward", "scroll_backward", "open_notification" -> {
            if (action.nodeId.isBlank() || action.text.isNotEmpty()) {
                throw RequestException(400, "node action requires node_id and no text")
            }
            if (action.kind != "open_notification" && action.expectedSequence <= 0) {
                throw RequestException(400, "node action requires expected_sequence")
            }
        }
        "set_text" -> {
            if (action.nodeId.isBlank() || action.text.length > 32 * 1024 || action.expectedSequence <= 0) {
                throw RequestException(400, "invalid set_text action")
            }
        }
        "global_back" -> {
            if (action.nodeId.isNotEmpty() || action.text.isNotEmpty() || action.expectedSequence <= 0) {
                throw RequestException(400, "global_back requires expected_sequence and no node or text")
            }
        }
        else -> throw RequestException(400, "unsupported action")
    }
}

internal data class ParsedHttpRequest(
    val method: String,
    val path: String,
    val query: Map<String, String>,
    val headers: Map<String, String>,
    val body: ByteArray,
)

internal object HttpRequestParser {
    private const val MAX_LINE_BYTES = 8 * 1024
    private const val MAX_HEADER_BYTES = 16 * 1024
    private const val MAX_BODY_BYTES = 64 * 1024

    fun parse(input: BufferedInputStream): ParsedHttpRequest {
        var headerBytes = 0
        val requestLine = readLine(input).also { headerBytes += it.length }
        val requestParts = requestLine.split(' ')
        if (requestParts.size != 3 || requestParts[2] != "HTTP/1.1") {
            throw RequestException(400, "invalid HTTP request line")
        }
        if (requestParts[0] != "GET" && requestParts[0] != "POST") {
            throw RequestException(400, "unsupported HTTP method")
        }
        val headers = LinkedHashMap<String, String>()
        while (true) {
            val line = readLine(input)
            headerBytes += line.length
            if (headerBytes > MAX_HEADER_BYTES) throw RequestException(400, "headers too large")
            if (line.isEmpty()) break
            val separator = line.indexOf(':')
            if (separator < 1) throw RequestException(400, "invalid HTTP header")
            val name = line.substring(0, separator).trim().lowercase(Locale.US)
            val value = line.substring(separator + 1).trim()
            if (headers.put(name, value) != null) throw RequestException(400, "duplicate HTTP header")
        }
        val contentLength = headers["content-length"]?.toIntOrNull() ?: 0
        if (contentLength < 0 || contentLength > MAX_BODY_BYTES) {
            throw RequestException(400, "request body too large")
        }
        val body = ByteArray(contentLength)
        var offset = 0
        while (offset < body.size) {
            val count = input.read(body, offset, body.size - offset)
            if (count < 0) throw RequestException(400, "truncated request body")
            offset += count
        }
        val target = requestParts[1]
        val queryStart = target.indexOf('?')
        val path = if (queryStart >= 0) target.substring(0, queryStart) else target
        if (!path.startsWith('/') || path.contains("..")) throw RequestException(400, "invalid request path")
        val query = if (queryStart >= 0) parseQuery(target.substring(queryStart + 1)) else emptyMap()
        return ParsedHttpRequest(requestParts[0], path, query, headers, body)
    }

    private fun readLine(input: BufferedInputStream): String {
        val bytes = ArrayList<Byte>()
        while (bytes.size <= MAX_LINE_BYTES) {
            val value = input.read()
            if (value < 0) throw EOFException("unexpected end of request")
            if (value == '\n'.code) {
                if (bytes.isEmpty() || bytes.last() != '\r'.code.toByte()) {
                    throw RequestException(400, "HTTP line must use CRLF")
                }
                bytes.removeAt(bytes.lastIndex)
                return bytes.toByteArray().toString(StandardCharsets.US_ASCII)
            }
            bytes.add(value.toByte())
        }
        throw RequestException(400, "HTTP line too long")
    }

    private fun parseQuery(raw: String): Map<String, String> {
        if (raw.isEmpty()) return emptyMap()
        return raw.split('&').associate { part ->
            val separator = part.indexOf('=')
            val key = if (separator >= 0) part.substring(0, separator) else part
            val value = if (separator >= 0) part.substring(separator + 1) else ""
            URLDecoder.decode(key, "UTF-8") to URLDecoder.decode(value, "UTF-8")
        }
    }
}

internal class RequestException(val status: Int, message: String) : RuntimeException(message)
