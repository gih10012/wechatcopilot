package dev.wechatcopilot.companion

import java.io.BufferedInputStream
import java.io.ByteArrayInputStream
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class ProtocolTest {
    @Test
    fun tokenPolicyRequiresStrongUrlSafeToken() {
        assertTrue(TokenPolicy.isValid("abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"))
        assertFalse(TokenPolicy.isValid("short"))
        assertFalse(TokenPolicy.isValid("abcdefghijklmnopqrstuvwxyzABCDEFGH0123456!89"))
    }

    @Test
    fun parserAcceptsBoundedJsonRequest() {
        val body = "{\"kind\":\"global_back\"}"
        val wire = buildString {
            append("POST /v1/actions HTTP/1.1\r\n")
            append("Authorization: Bearer token\r\n")
            append("Content-Length: ${body.toByteArray().size}\r\n")
            append("\r\n")
            append(body)
        }
        val parsed = HttpRequestParser.parse(BufferedInputStream(ByteArrayInputStream(wire.toByteArray())))
        assertEquals("POST", parsed.method)
        assertEquals("/v1/actions", parsed.path)
        assertEquals(body, parsed.body.toString(Charsets.UTF_8))
    }

    @Test
    fun parserRejectsDuplicateHeaders() {
        val wire = "GET /v1/health HTTP/1.1\r\nAuthorization: a\r\nAuthorization: b\r\n\r\n"
        assertFailsWith<RequestException> {
            HttpRequestParser.parse(BufferedInputStream(ByteArrayInputStream(wire.toByteArray())))
        }
    }
}
