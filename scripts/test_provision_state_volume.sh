#!/usr/bin/env bash
set -euo pipefail

readonly script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly provision_script="$script_dir/provision_state_volume.sh"

fail_test() {
    printf 'test failure: %s\n' "$*" >&2
    exit 1
}

test_create_exit_cleanup() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/backing/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    backing_fs_uuid="fixture"
    owner_name=$(id -un)
    confirm_create=true
    size_gib=0

    validate_arguments() { :; }
    require_root() { :; }
    acquire_operation_lock() { :; }
    require_tty() { :; }
    require_commands() { :; }
    require_backing_uuid() { :; }
    run_preflight() { :; }
    df() { printf 'Avail\n999999999999\n'; }
    fallocate() {
        local output=${*: -1}
        : >"$output"
    }
    sync() { :; }
    cryptsetup() {
        case "${1:-}" in
            luksFormat)
                : >"$local_root/luks-format.called"
                exit 1
                ;;
            isLuks)
                return 1
                ;;
            *)
                return 1
                ;;
        esac
    }

    error_log="$local_root/create.err"
    if create_volume > /dev/null 2>"$error_log"; then
        fail_test "create fixture unexpectedly succeeded"
    fi
    if find "$local_root/backing" -maxdepth 1 -type f -name '.state.luks.creating.*' -print -quit | grep -q .; then
        fail_test "failed create left an unformatted staging image"
    fi
    [[ -e "$local_root/luks-format.called" ]] || fail_test "create fixture did not reach luksFormat"
    ! grep -q 'unbound variable' "$error_log" || fail_test "create EXIT cleanup lost its local state"
)

test_create_retains_detected_luks_header() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/backing/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    backing_fs_uuid="fixture"
    owner_name=$(id -un)
    confirm_create=true
    size_gib=0

    validate_arguments() { :; }
    require_root() { :; }
    acquire_operation_lock() { :; }
    require_tty() { :; }
    require_commands() { :; }
    require_backing_uuid() { :; }
    run_preflight() { :; }
    df() { printf 'Avail\n999999999999\n'; }
    fallocate() {
        local output=${*: -1}
        : >"$output"
    }
    sync() { :; }
    cryptsetup() {
        case "${1:-}" in
            luksFormat)
                : >"$local_root/luks-header-written"
                exit 1
                ;;
            isLuks)
                [[ -e "$local_root/luks-header-written" ]]
                ;;
            *)
                return 1
                ;;
        esac
    }

    error_log="$local_root/create.err"
    if create_volume > /dev/null 2>"$error_log"; then
        fail_test "LUKS-header fixture unexpectedly succeeded"
    fi
    retained=$(find "$local_root/backing" -maxdepth 1 -type f -name '.state.luks.creating.*' -print -quit)
    [[ -n "$retained" && -e "$retained" ]] || fail_test "cleanup deleted a staging image with a detected LUKS2 header"
    grep -q 'retained encrypted staging image for diagnosis' "$error_log" || fail_test "retained staging image was not reported"
    ! grep -q 'unbound variable' "$error_log" || fail_test "LUKS-header cleanup lost its local state"
)

test_create_retains_staging_on_luks_probe_error() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/backing/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    backing_fs_uuid="fixture"
    owner_name=$(id -un)
    confirm_create=true
    size_gib=0

    validate_arguments() { :; }
    require_root() { :; }
    acquire_operation_lock() { :; }
    require_tty() { :; }
    require_commands() { :; }
    require_backing_uuid() { :; }
    run_preflight() { :; }
    df() { printf 'Avail\n999999999999\n'; }
    fallocate() {
        local output=${*: -1}
        : >"$output"
    }
    sync() { :; }
    cryptsetup() {
        case "${1:-}" in
            luksFormat)
                exit 1
                ;;
            isLuks)
                return 2
                ;;
            *)
                return 1
                ;;
        esac
    }

    error_log="$local_root/create.err"
    if create_volume > /dev/null 2>"$error_log"; then
        fail_test "LUKS-probe-error fixture unexpectedly succeeded"
    fi
    retained=$(find "$local_root/backing" -maxdepth 1 -type f -name '.state.luks.creating.*' -print -quit)
    [[ -n "$retained" && -e "$retained" ]] || fail_test "cleanup deleted staging after an inconclusive LUKS probe"
    grep -q 'LUKS probe failed with status 2' "$error_log" || fail_test "inconclusive LUKS probe was not reported"
)

test_create_rejects_orphan_staging() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/backing/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    backing_fs_uuid="fixture"
    owner_name=$(id -un)
    confirm_create=true
    size_gib=0
    mkdir -p "$(dirname "$backing_file")"
    orphan="$(dirname "$backing_file")/.state.luks.creating.orphan"
    : >"$orphan"

    validate_arguments() { :; }
    require_root() { :; }
    acquire_operation_lock() { :; }
    require_tty() { :; }
    require_commands() { :; }
    require_backing_uuid() { :; }
    run_preflight() { :; }

    error_log="$local_root/create.err"
    if create_volume 2>"$error_log"; then
        fail_test "create accepted an orphan staging image"
    fi
    [[ -e "$orphan" ]] || fail_test "create removed an orphan without explicit verification"
    grep -Fq "$orphan" "$error_log" || fail_test "orphan rejection did not report the exact path"
)

test_create_closes_mapper_after_open_error() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/backing/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    backing_fs_uuid="fixture"
    owner_name=$(id -un)
    confirm_create=true
    size_gib=0

    validate_arguments() { :; }
    require_root() { :; }
    acquire_operation_lock() { :; }
    require_tty() { :; }
    require_commands() { :; }
    require_backing_uuid() { :; }
    run_preflight() { :; }
    df() { printf 'Avail\n999999999999\n'; }
    fallocate() {
        local output=${*: -1}
        : >"$output"
    }
    sync() { :; }
    mountpoint() { return 1; }
    verify_mapping_identity() { [[ -e "$local_root/mapper-active" ]]; }
    verify_backing_loop_set() { return 0; }
    cryptsetup() {
        case "${1:-}" in
            luksFormat)
                return 0
                ;;
            open)
                : >"$local_root/mapper-active"
                exit 1
                ;;
            status)
                [[ -e "$local_root/mapper-active" ]]
                ;;
            close)
                rm -f -- "$local_root/mapper-active"
                : >"$local_root/mapper-closed"
                ;;
            isLuks)
                return 0
                ;;
            *)
                return 1
                ;;
        esac
    }

    error_log="$local_root/create.err"
    if create_volume > /dev/null 2>"$error_log"; then
        fail_test "mapper-open fixture unexpectedly succeeded"
    fi
    [[ -e "$local_root/mapper-closed" && ! -e "$local_root/mapper-active" ]] || fail_test "cleanup did not close a mapper left by failed open"
    retained=$(find "$local_root/backing" -maxdepth 1 -type f -name '.state.luks.creating.*' -print -quit)
    [[ -n "$retained" ]] || fail_test "cleanup discarded the formatted staging image after failed open"
    ! grep -q 'unbound variable' "$error_log" || fail_test "mapper-open cleanup lost its local state"
)

test_create_unmounts_after_mount_error() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/backing/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    backing_fs_uuid="fixture"
    owner_name=$(id -un)
    confirm_create=true
    size_gib=0

    validate_arguments() { :; }
    require_root() { :; }
    acquire_operation_lock() { :; }
    require_tty() { :; }
    require_commands() { :; }
    require_backing_uuid() { :; }
    run_preflight() { :; }
    df() { printf 'Avail\n999999999999\n'; }
    fallocate() {
        local output=${*: -1}
        : >"$output"
    }
    sync() { :; }
    verify_mapping_backing() { :; }
    verify_mapping_identity() { [[ -e "$local_root/mapper-active" ]]; }
    verify_backing_loop_set() { return 0; }
    mount_source_matches() { [[ -e "$local_root/mount-active" ]]; }
    mountpoint() { [[ -e "$local_root/mount-active" ]]; }
    blkid() { return 0; }
    mkfs.ext4() { :; }
    install() {
        local output=${*: -1}
        if [[ "$output" == "$mount_point" ]]; then
            mkdir -p "$mount_point"
            chmod 0700 "$mount_point"
        else
            command install "$@"
        fi
    }
    stat() {
        local output=${*: -1}
        if [[ "$output" == "$mount_point" && "$*" == *"%u:%a"* ]]; then
            printf '0:700\n'
        else
            command stat "$@"
        fi
    }
    mount() {
        : >"$local_root/mount-active"
        exit 1
    }
    umount() {
        rm -f -- "$local_root/mount-active"
        : >"$local_root/mount-removed"
    }
    cryptsetup() {
        case "${1:-}" in
            luksFormat)
                return 0
                ;;
            open)
                : >"$local_root/mapper-active"
                ;;
            status)
                [[ -e "$local_root/mapper-active" ]]
                ;;
            close)
                rm -f -- "$local_root/mapper-active"
                : >"$local_root/mapper-closed"
                ;;
            isLuks)
                return 0
                ;;
            *)
                return 1
                ;;
        esac
    }

    error_log="$local_root/create.err"
    if create_volume > /dev/null 2>"$error_log"; then
        fail_test "mount fixture unexpectedly succeeded"
    fi
    [[ -e "$local_root/mount-removed" && ! -e "$local_root/mount-active" ]] || fail_test "cleanup did not unmount a mount left by failed mount"
    [[ -e "$local_root/mapper-closed" && ! -e "$local_root/mapper-active" ]] || fail_test "cleanup did not close the mapper after failed mount"
    ! grep -q 'unbound variable' "$error_log" || fail_test "mount cleanup lost its local state"
)

test_create_reports_final_verification_recovery() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/backing/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    backing_fs_uuid="fixture"
    owner_name=$(id -un)
    confirm_create=true
    size_gib=0

    validate_arguments() { :; }
    require_root() { :; }
    acquire_operation_lock() { :; }
    require_tty() { :; }
    require_commands() { :; }
    require_backing_uuid() { :; }
    run_preflight() { :; }
    df() { printf 'Avail\n999999999999\n'; }
    fallocate() {
        local output=${*: -1}
        : >"$output"
    }
    sync() { :; }
    verify_mapping_backing() { :; }
    verify_mapping_identity() { [[ -e "$local_root/mapper-active" ]]; }
    verify_backing_loop_set() { return 0; }
    verify_backing_file() { :; }
    verify_mounted_volume() { :; }
    mount_source_matches() { [[ -e "$local_root/mount-active" ]]; }
    mountpoint() { [[ -e "$local_root/mount-active" ]]; }
    blkid() {
        if [[ "$*" == *"-p"* ]]; then
            return 0
        fi
        printf '00112233-4455-6677-8899-aabbccddeeff\n'
    }
    mkfs.ext4() { :; }
    install() {
        local output=${*: -1}
        case "$output" in
            "$mount_point")
                mkdir -p "$mount_point"
                chmod 0700 "$mount_point"
                ;;
            "$mount_point/.wechatcopilot-volume")
                : >"$output"
                chmod 0600 "$output"
                ;;
            *)
                command install "$@"
                ;;
        esac
    }
    stat() {
        local output=${*: -1}
        if [[ "$output" == "$mount_point" && "$*" == *"%u:%a"* ]]; then
            printf '0:700\n'
        else
            command stat "$@"
        fi
    }
    chown() { :; }
    mount() { : >"$local_root/mount-active"; }
    umount() { rm -f -- "$local_root/mount-active"; }
    install_system_config() { : >"$local_root/config-installed"; }
    start_volume_units() { exit 1; }
    cryptsetup() {
        case "${1:-}" in
            luksFormat)
                return 0
                ;;
            open)
                : >"$local_root/mapper-active"
                ;;
            status)
                [[ -e "$local_root/mapper-active" ]]
                ;;
            close)
                rm -f -- "$local_root/mapper-active"
                : >"$local_root/mapper-closed"
                ;;
            luksUUID)
                printf '11111111-2222-3333-4444-555555555555\n'
                ;;
            isLuks)
                return 0
                ;;
            *)
                return 1
                ;;
        esac
    }

    error_log="$local_root/create.err"
    if create_volume > /dev/null 2>"$error_log"; then
        fail_test "final-verification fixture unexpectedly succeeded"
    fi
    [[ -f "$backing_file" ]] || fail_test "post-rename failure lost the completed backing image"
    if find "$(dirname "$backing_file")" -maxdepth 1 -type f -name '.state.luks.creating.*' -print -quit | grep -q .; then
        fail_test "post-rename failure left a stale staging name"
    fi
    [[ ! -e "$local_root/mount-active" && ! -e "$local_root/mapper-active" && -e "$local_root/mapper-closed" ]] || fail_test "post-rename cleanup left resources active"
    [[ -e "$local_root/config-installed" ]] || fail_test "fixture failed before persistent configuration"
    grep -Fq "retained completed encrypted image: $backing_file" "$error_log" || fail_test "post-rename failure did not report the retained final image"
    grep -q 'rerun configure with the same options' "$error_log" || fail_test "post-rename failure did not explain recovery"
)

test_configure_recovers_locked_final_image() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/backing/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    backing_fs_uuid="fixture"
    owner_name=$(id -un)
    mkdir -p "$(dirname "$backing_file")"
    : >"$backing_file"

    validate_arguments() { :; }
    require_root() { :; }
    acquire_operation_lock() { :; }
    require_commands() { :; }
    require_backing_uuid() { :; }
    run_preflight() { :; }
    verify_backing_file() { : >"$local_root/backing-verified"; }
    mapper_active() { return 1; }
    require_tty() { : >"$local_root/tty-required"; }
    verify_backing_loop_set() { : >"$local_root/loop-verified"; }
    mountpoint() { return 1; }
    install_system_config() { : >"$local_root/config-installed"; }
    start_volume_units() { : >"$local_root/volume-started"; }
    verify_mounted_volume() { : >"$local_root/mount-verified"; }
    blkid() { printf '00112233-4455-6677-8899-aabbccddeeff\n'; }

    configure_volume >"$local_root/configure.out"
    for marker in backing-verified tty-required loop-verified config-installed volume-started mount-verified; do
        [[ -e "$local_root/$marker" ]] || fail_test "locked-image recovery skipped $marker"
    done
    grep -q 'WECHATCOPILOT_STATE_MOUNT_UUID=00112233-4455-6677-8899-aabbccddeeff' "$local_root/configure.out" || fail_test "recovery did not report the verified inner UUID"
)

test_unlock_exit_cleanup() (
    set -- help
    # shellcheck source=provision_state_volume.sh
    source "$provision_script" >/dev/null

    local_root=$(mktemp -d)
    trap 'rm -rf -- "$local_root"' EXIT
    backing_file="$local_root/state.luks"
    mount_point="$local_root/mount"
    mapper_name="wechatcopilot-cleanup-fixture"
    : >"$backing_file"

    systemd-escape() {
        if [[ "$*" == *systemd-cryptsetup* ]]; then
            printf 'fixture-crypt.service\n'
        else
            printf 'fixture.mount\n'
        fi
    }
    verify_system_config() { :; }
    mapper_active() { return 1; }
    verify_backing_loop_set() { :; }
    mountpoint() { return 1; }
    systemctl() {
        if [[ "${1:-}" == show ]]; then
            printf 'not-found\n'
        fi
        return 0
    }

    error_log="$local_root/unlock.err"
    if start_volume_units 2>"$error_log"; then
        fail_test "unlock fixture unexpectedly succeeded"
    fi
    grep -q 'is not loaded' "$error_log" || fail_test "unlock fixture failed at the wrong stage"
    ! grep -q 'unbound variable' "$error_log" || fail_test "unlock EXIT cleanup lost its local state"
)

test_create_exit_cleanup
test_create_retains_detected_luks_header
test_create_retains_staging_on_luks_probe_error
test_create_rejects_orphan_staging
test_create_closes_mapper_after_open_error
test_create_unmounts_after_mount_error
test_create_reports_final_verification_recovery
test_configure_recovers_locked_final_image
test_unlock_exit_cleanup
printf 'state-volume cleanup tests passed\n'
