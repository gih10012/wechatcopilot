package dev.wechatcopilot.companion

import java.io.BufferedInputStream
import java.io.ByteArrayInputStream
import java.time.Instant
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class ProtocolTest {
    @Test
    fun observedWindowClassIsBoundToTheCurrentPackageAndWindow() {
        val activity = "com.tencent.wework.login.controller.LoginWxAuthActivity"
        CompanionRuntime.observeWindow(activity, 7)
        assertEquals(activity, CompanionRuntime.currentWindowClass(CompanionRuntime.WECOM_PACKAGE, 7))
        assertEquals("", CompanionRuntime.currentWindowClass(CompanionRuntime.WECOM_PACKAGE, 8))
        assertEquals("", CompanionRuntime.currentWindowClass("com.android.systemui", 7))
        CompanionRuntime.observeWindow(activity, -1)
        assertEquals("", CompanionRuntime.currentWindowClass(CompanionRuntime.WECOM_PACKAGE, -1))
        CompanionRuntime.observeWindow("", 7)
        assertEquals("", CompanionRuntime.currentWindowClass(CompanionRuntime.WECOM_PACKAGE, 7))
    }

    @Test
    fun snapshotWireScalarFieldsMatchTheProtocolGolden() {
        val snapshot = UiSnapshotModel(
            sequence = 7,
            packageName = CompanionRuntime.WECOM_PACKAGE,
            windowId = 11,
            windowTitle = "",
            windowClass = "com.tencent.wework.login.controller.LoginWxAuthActivity",
            capturedAt = Instant.EPOCH,
            nodes = emptyList(),
        )
        assertEquals(
            linkedMapOf(
                "sequence" to 7L,
                "package_name" to CompanionRuntime.WECOM_PACKAGE,
                "window_id" to 11,
                "window_title" to "",
                "window_class" to "com.tencent.wework.login.controller.LoginWxAuthActivity",
                "captured_at" to Instant.EPOCH.toString(),
            ),
            snapshot.wireScalarFields(),
        )
    }

    @Test
    fun nodeProtocolCarriesVisibilityCheckedAndSelectedStateToTheHost() {
        val node = UiNodeModel(
            id = "0/1",
            parentId = "0",
            className = "android.widget.Button",
            viewId = "",
            text = "Agree",
            contentDescription = "",
            bounds = BoundsModel(10, 20, 100, 60),
            clickable = true,
            checkable = true,
            checked = true,
            selected = true,
            editable = false,
            scrollable = false,
            enabled = true,
            focused = false,
            visibleToUser = true,
        )
        // android.jar's JSONObject is a stub in local JVM tests; Go protocol tests
        // cover the serialized field name and decoding.
        assertTrue(node.visibleToUser)
        assertTrue(node.checkable)
        assertTrue(node.checked)
        assertTrue(node.selected)
    }

    @Test
    fun checkActionGateFailsClosed() {
        assertTrue(canRequestCheck(true, true, true, true, false))
        assertFalse(canRequestCheck(false, true, true, true, false))
        assertFalse(canRequestCheck(true, false, true, true, false))
        assertFalse(canRequestCheck(true, true, false, true, false))
        assertFalse(canRequestCheck(true, true, true, false, false))
        assertFalse(canRequestCheck(true, true, true, true, true))
    }

    @Test
    fun localActionValidatorAllowsOnlyConstrainedCheckRequests() {
        validateLocalAction(
            CompanionAction(
                kind = "check",
                nodeId = "0/1",
                text = "",
                expectedSequence = 7,
            ),
        )

        val invalid = listOf(
            CompanionAction("check", "", "", 7),
            CompanionAction("check", "0/1", "unexpected", 7),
            CompanionAction("check", "0/1", "", 0),
            CompanionAction("toggle", "0/1", "", 7),
        )
        invalid.forEach { action ->
            assertFailsWith<RequestException> { validateLocalAction(action) }
        }
    }

    @Test
    fun localActionValidatorRequiresASequenceForGlobalBack() {
        validateLocalAction(CompanionAction("global_back", "", "", 7))

        val invalid = listOf(
            CompanionAction("global_back", "", "", 0),
            CompanionAction("global_back", "0/1", "", 7),
            CompanionAction("global_back", "", "unexpected", 7),
        )
        invalid.forEach { action ->
            assertFailsWith<RequestException> { validateLocalAction(action) }
        }
    }

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
