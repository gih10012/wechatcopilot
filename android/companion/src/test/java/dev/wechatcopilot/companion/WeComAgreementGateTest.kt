package dev.wechatcopilot.companion

import kotlin.test.Test
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class WeComAgreementGateTest {
    @Test
    fun exactOfficialUnselectedImageViewIsAllowed() {
        val fingerprint = loginFingerprint(selected = false)

        assertTrue(canRequestWeComLoginAgreementSelection(fingerprint, fingerprint.nodes[3]))
        assertTrue(constrainedCheck(fingerprint))
    }

    @Test
    fun selectedImageViewCannotBeRequestedAgain() {
        val fingerprint = loginFingerprint(selected = true)

        assertFalse(canRequestWeComLoginAgreementSelection(fingerprint, fingerprint.nodes[3]))
        assertFalse(constrainedCheck(fingerprint))
        assertFalse(constrainedCheck(fingerprint, selected = true))
    }

    @Test
    fun liveSelectedStateCannotRaceAnUnselectedFingerprint() {
        val fingerprint = loginFingerprint(selected = false)

        assertFalse(constrainedCheck(fingerprint, selected = true))
    }

    @Test
    fun agreementImageViewCannotUseTheGenericClickAction() {
        listOf(false, true).forEach { selected ->
            val fingerprint = loginFingerprint(selected)
            assertFalse(constrainedClick(fingerprint))
        }

        val fingerprint = loginFingerprint(selected = false)
        assertFalse(
            constrainedClick(
                fingerprint.withTarget { it.copy(className = "android.widget.Button") },
            ),
        )
        assertFalse(
            constrainedClick(
                fingerprint,
                liveClassName = "android.widget.Button",
                liveViewId = "",
            ),
        )
    }

    @Test
    fun unrelatedImageViewCannotUseCheckButKeepsGenericClick() {
        val fingerprint = loginFingerprint(selected = false).withTarget {
            it.copy(viewId = "com.tencent.wework:id/avatar")
        }

        assertFalse(
            constrainedCheck(
                fingerprint,
                liveViewId = "com.tencent.wework:id/avatar",
            ),
        )
        assertTrue(
            constrainedClick(
                fingerprint,
                liveViewId = "com.tencent.wework:id/avatar",
            ),
        )
    }

    @Test
    fun standardCheckableControlKeepsTheExistingGate() {
        val fingerprint = loginFingerprint(selected = false).withTarget {
            it.copy(
                className = "android.widget.CheckBox",
                viewId = "other:id/checkbox",
                checkable = true,
            )
        }
        val target = fingerprint.nodes[3]

        assertTrue(
            canRequestConstrainedCheck(
                fingerprint = fingerprint,
                target = target,
                packageName = CompanionRuntime.WECOM_PACKAGE,
                className = "android.widget.CheckBox",
                viewId = "other:id/checkbox",
                visibleToUser = true,
                enabled = true,
                clickable = true,
                checkable = true,
                checked = false,
                selected = false,
            ),
        )
        assertFalse(
            canRequestConstrainedCheck(
                fingerprint = fingerprint,
                target = target,
                packageName = CompanionRuntime.WECOM_PACKAGE,
                className = "android.widget.CheckBox",
                viewId = "other:id/checkbox",
                visibleToUser = true,
                enabled = true,
                clickable = true,
                checkable = true,
                checked = true,
                selected = false,
            ),
        )
    }

    @Test
    fun customAgreementGateRejectsAnyIdentityStateOrContextDrift() {
        val original = loginFingerprint(selected = false)
        val transforms = listOf<(SemanticTreeFingerprint) -> SemanticTreeFingerprint>(
            { it.copy(packageName = "other.package") },
            { it.copy(windowClass = "other.Activity") },
            { it.copy(complete = false) },
            { it.withTarget { node -> node.copy(className = "android.widget.CheckBox") } },
            { it.withTarget { node -> node.copy(viewId = "com.tencent.wework:id/other") } },
            { it.withTarget { node -> node.copy(visibleToUser = false) } },
            { it.withTarget { node -> node.copy(enabled = false) } },
            { it.withTarget { node -> node.copy(clickable = false) } },
            { it.withTarget { node -> node.copy(checkable = true) } },
            { it.withTarget { node -> node.copy(checked = true) } },
            { it.withTarget { node -> node.copy(bounds = BoundsModel(0, 0, 0, 0)) } },
            { value -> value.copy(nodes = value.nodes + value.nodes[3].copy(id = "0/5")) },
            { it.withNode(0) { node -> node.copy(text = "") } },
            { it.withNode(1) { node -> node.copy(text = "") } },
            { it.withNode(2) { node -> node.copy(text = "") } },
            { it.withNode(4) { node -> node.copy(text = "Read and Agree Privacy Policy") } },
            { value -> value.copy(nodes = value.nodes + value.nodes[0].copy(id = "0/5")) },
            { value -> value.copy(nodes = value.nodes + markerNode("0/5", "Device verification")) },
            { value -> value.copy(nodes = value.nodes + markerNode("0/5", "Agree")) },
            { value -> value.copy(nodes = value.nodes + markerNode("0/5", "Welcome to WeCom")) },
        )

        transforms.forEachIndexed { index, transform ->
            val changed = transform(original)
            assertFalse(
                canRequestWeComLoginAgreementSelection(changed, changed.nodes[3]),
                "unsafe transform $index was accepted",
            )
        }
        assertFalse(canRequestWeComLoginAgreementSelection(original, original.nodes[0]))
    }

    @Test
    fun customAgreementGateRejectsEveryHostRecognizedHardRiskMarker() {
        val original = loginFingerprint(selected = false)
        val markers = listOf(
            "账号存在风险",
            "账户存在风险",
            "设备验证",
            "安全验证",
            "异常登录",
            "确认本人操作",
            "人脸验证",
            "人脸识别",
            "滑块验证",
            "完成验证",
            "Account risk",
            "Device verification",
            "Security verification",
            "Unusual login",
            "Confirm on your phone",
            "Face verification",
            "Captcha",
        )

        markers.forEachIndexed { index, marker ->
            val changed = original.copy(nodes = original.nodes + markerNode("0/risk-$index", marker))
            assertFalse(
                canRequestWeComLoginAgreementSelection(changed, changed.nodes[3]),
                "hard-risk marker $marker was accepted",
            )
        }
    }

    @Test
    fun loginLabelsMayResolveToDistinctClickableAncestors() {
        val original = loginFingerprint(selected = false)
        val nested = original.copy(
            nodes = listOf(
                original.nodes[0].copy(id = "0/0/0", parentId = "0/0", clickable = false),
                original.nodes[1].copy(id = "0/1/0", parentId = "0/1", clickable = false),
                original.nodes[2].copy(id = "0/2/0", parentId = "0/2", clickable = false),
                original.nodes[3],
                original.nodes[4],
                buttonContainer("0/0"),
                buttonContainer("0/1"),
                buttonContainer("0/2"),
            ),
        )

        assertTrue(canRequestWeComLoginAgreementSelection(nested, nested.nodes[3]))
    }

    @Test
    fun loginLabelsCannotShareOneClickableAncestor() {
        val original = loginFingerprint(selected = false)
        val shared = buttonContainer("0/shared")
        val changed = original.copy(
            nodes = listOf(
                original.nodes[0].copy(id = "0/shared/0", parentId = shared.id, clickable = false),
                original.nodes[1].copy(id = "0/shared/1", parentId = shared.id, clickable = false),
                original.nodes[2],
                original.nodes[3],
                original.nodes[4],
                shared,
            ),
        )

        assertFalse(canRequestWeComLoginAgreementSelection(changed, changed.nodes[3]))
    }

    private fun loginFingerprint(selected: Boolean) = SemanticTreeFingerprint(
        packageName = CompanionRuntime.WECOM_PACKAGE,
        windowId = 7,
        windowTitle = "WeCom",
        windowClass = "com.tencent.wework.login.controller.LoginWxAuthActivity",
        complete = true,
        nodes = listOf(
            buttonNode("0/0", "Continue with WeChat"),
            buttonNode("0/1", "Continue with Email"),
            buttonNode("0/2", "Continue with Phone"),
            UiNodeModel(
                id = "0/3",
                parentId = "0",
                className = "android.widget.ImageView",
                viewId = "com.tencent.wework:id/ow",
                text = "",
                contentDescription = "",
                bounds = BoundsModel(20, 460, 50, 490),
                clickable = true,
                checkable = false,
                checked = false,
                selected = selected,
                editable = false,
                scrollable = false,
                enabled = true,
                focused = false,
                visibleToUser = true,
            ),
            markerNode(
                "0/4",
                "Read and Agree Software Licensing and Service Agreements and Privacy Policy",
            ),
        ),
    )

    private fun buttonNode(id: String, text: String) = UiNodeModel(
        id = id,
        parentId = "0",
        className = "android.widget.Button",
        viewId = "",
        text = text,
        contentDescription = "",
        bounds = BoundsModel(20, 300, 340, 360),
        clickable = true,
        checkable = false,
        checked = false,
        selected = false,
        editable = false,
        scrollable = false,
        enabled = true,
        focused = false,
        visibleToUser = true,
    )

    private fun buttonContainer(id: String) = buttonNode(id, "").copy(
        className = "android.view.View",
    )

    private fun constrainedCheck(
        fingerprint: SemanticTreeFingerprint,
        checkable: Boolean = false,
        checked: Boolean = false,
        selected: Boolean = false,
        liveClassName: String = "android.widget.ImageView",
        liveViewId: String = "com.tencent.wework:id/ow",
    ): Boolean = canRequestConstrainedCheck(
        fingerprint = fingerprint,
        target = fingerprint.nodes[3],
        packageName = CompanionRuntime.WECOM_PACKAGE,
        className = liveClassName,
        viewId = liveViewId,
        visibleToUser = true,
        enabled = true,
        clickable = true,
        checkable = checkable,
        checked = checked,
        selected = selected,
    )

    private fun constrainedClick(
        fingerprint: SemanticTreeFingerprint,
        liveClassName: String = "android.widget.ImageView",
        liveViewId: String = "com.tencent.wework:id/ow",
    ): Boolean = canRequestConstrainedClick(
        fingerprint = fingerprint,
        target = fingerprint.nodes[3],
        packageName = CompanionRuntime.WECOM_PACKAGE,
        className = liveClassName,
        viewId = liveViewId,
        visibleToUser = true,
        enabled = true,
        clickable = true,
    )

    private companion object {
        fun markerNode(id: String, text: String) = UiNodeModel(
            id = id,
            parentId = "0",
            className = "android.widget.TextView",
            viewId = "",
            text = text,
            contentDescription = "",
            bounds = BoundsModel(60, 450, 340, 510),
            clickable = false,
            checkable = false,
            checked = false,
            selected = false,
            editable = false,
            scrollable = false,
            enabled = true,
            focused = false,
            visibleToUser = true,
        )
    }
}

private fun SemanticTreeFingerprint.withTarget(
    transform: (UiNodeModel) -> UiNodeModel,
): SemanticTreeFingerprint = withNode(3, transform)

private fun SemanticTreeFingerprint.withNode(
    index: Int,
    transform: (UiNodeModel) -> UiNodeModel,
): SemanticTreeFingerprint {
    val changed = nodes.toMutableList()
    changed[index] = transform(changed[index])
    return copy(nodes = changed)
}
