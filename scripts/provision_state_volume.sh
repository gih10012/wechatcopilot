#!/usr/bin/env bash
set -euo pipefail
set +x
shopt -s inherit_errexit
umask 077
export LC_ALL=C

readonly GIB=$((1024 * 1024 * 1024))
readonly CONFIG_BEGIN="# BEGIN wechatcopilot state volume"
readonly CONFIG_END="# END wechatcopilot state volume"

command_name="${1:-help}"
if [[ $# -gt 0 ]]; then
    shift
fi

backing_file="/share/wechatcopilot-state/state.luks"
backing_fs_uuid=""
mount_point="/srv/wechatcopilot-state"
mapper_name="wechatcopilot-state"
size_gib=64
owner_name="${SUDO_USER:-${USER:-}}"
confirm_create=false
confirm_daemon_stopped=false

usage() {
    cat <<'EOF'
Usage:
  provision_state_volume.sh preflight [options]
  provision_state_volume.sh create --confirm-create [options]
  provision_state_volume.sh configure [options]
  provision_state_volume.sh unlock [options]
  provision_state_volume.sh lock --confirm-daemon-stopped [options]
  provision_state_volume.sh status [options]

Options:
  --backing-file PATH       LUKS2 file (default: /share/wechatcopilot-state/state.luks)
  --backing-fs-uuid UUID    Expected filesystem UUID containing the backing file
  --mount-point PATH        Decrypted ext4 mount (default: /srv/wechatcopilot-state)
  --mapper-name NAME        dm-crypt mapper name (default: wechatcopilot-state)
  --size-gib N              Create/expected image size, at least 40 (default: 64)
  --owner USER              Daemon account owner (default: invoking sudo user)
  --confirm-create          Acknowledge allocation and formatting of a new file
  --confirm-daemon-stopped  Acknowledge the daemon was stopped before locking

No option accepts a passphrase. cryptsetup reads it directly from the terminal.
The script never deletes an existing encrypted image.
EOF
}

fail() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

note() {
    printf '%s\n' "$*"
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --backing-file)
            [[ $# -ge 2 ]] || fail "--backing-file requires a value"
            backing_file="$2"
            shift 2
            ;;
        --backing-fs-uuid)
            [[ $# -ge 2 ]] || fail "--backing-fs-uuid requires a value"
            backing_fs_uuid="$2"
            shift 2
            ;;
        --mount-point)
            [[ $# -ge 2 ]] || fail "--mount-point requires a value"
            mount_point="$2"
            shift 2
            ;;
        --mapper-name)
            [[ $# -ge 2 ]] || fail "--mapper-name requires a value"
            mapper_name="$2"
            shift 2
            ;;
        --size-gib)
            [[ $# -ge 2 ]] || fail "--size-gib requires a value"
            size_gib="$2"
            shift 2
            ;;
        --owner)
            [[ $# -ge 2 ]] || fail "--owner requires a value"
            owner_name="$2"
            shift 2
            ;;
        --confirm-create)
            confirm_create=true
            shift
            ;;
        --confirm-daemon-stopped)
            confirm_daemon_stopped=true
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "unknown option: $1"
            ;;
    esac
done

validate_arguments() {
    [[ "$backing_file" =~ ^/[A-Za-z0-9._/+:=-]+$ && "$(realpath -m -- "$backing_file")" == "$backing_file" ]] || fail "backing file must be a clean absolute path using fstab-safe characters"
    [[ "$mount_point" =~ ^/[A-Za-z0-9._/+:=-]+$ && "$(realpath -m -- "$mount_point")" == "$mount_point" ]] || fail "mount point must be a clean absolute path using fstab-safe characters"
    [[ "$backing_file" != / && "$mount_point" != / ]] || fail "backing file and mount point must not be the filesystem root"
    [[ "$backing_file" != "$mount_point" && "$backing_file" != "$mount_point/"* && "$mount_point" != "$backing_file/"* ]] || fail "backing file and decrypted mount point must not contain one another"
    [[ "$mapper_name" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$ ]] || fail "invalid mapper name"
    [[ "$size_gib" =~ ^[0-9]+$ ]] || fail "size must be an integer number of GiB"
    ((size_gib >= 40 && size_gib <= 512)) || fail "size must be between 40 and 512 GiB"
    [[ -z "$backing_fs_uuid" || "$backing_fs_uuid" =~ ^[A-Fa-f0-9-]{8,64}$ ]] || fail "invalid backing filesystem UUID"
    [[ "$owner_name" =~ ^[A-Za-z_][A-Za-z0-9_.-]{0,31}\$?$ && "$owner_name" != root ]] || fail "--owner must name a safe non-root daemon user"
    id -- "$owner_name" >/dev/null 2>&1 || fail "owner user does not exist: $owner_name"
    [[ ! -L "$backing_file" && ! -L "$mount_point" ]] || fail "backing file and mount point must not be symlinks"
}

require_backing_uuid() {
    [[ -n "$backing_fs_uuid" ]] || fail "$command_name requires --backing-fs-uuid"
}

require_root() {
    ((EUID == 0)) || fail "run this command through sudo from a trusted terminal"
}

require_tty() {
    [[ -r /dev/tty && -w /dev/tty && -t 0 ]] || fail "cryptsetup must run directly in an interactive terminal"
}

require_commands() {
    local tool losetup_help
    for tool in awk basename blkid cat chmod chown cmp cp cryptsetup df dirname env fallocate find findmnt flock grep id install losetup mkfs.ext4 mktemp mount mountpoint mv paste readlink realpath rm sha256sum sleep stat sync systemctl systemd-escape systemd-tty-ask-password-agent tail tr umount; do
        command -v "$tool" >/dev/null || fail "required command is missing: $tool"
    done
    losetup_help=$(losetup --help)
    [[ "$losetup_help" == *"BACK-INO"* && "$losetup_help" == *"BACK-MAJ:MIN"* ]] || fail "losetup is too old to verify the backing file identity"
}

acquire_operation_lock() {
	exec 9>/run/lock/wechatcopilot-state-volume.lock
	flock -n 9 || fail "another state-volume operation is running"
}

backing_mount_record() {
    local parent
    parent=$(dirname "$backing_file")
    while [[ ! -e "$parent" ]]; do
        parent=$(dirname "$parent")
    done
    findmnt -rn -T "$parent" -o SOURCE,TARGET,FSTYPE,UUID,OPTIONS | tail -n 1
}

check_backing_mount() {
    local teardown_mode="${1:-false}"
    local record source target fs_type actual_uuid options
    record=$(backing_mount_record)
    read -r source target fs_type actual_uuid options <<<"$record"
    [[ -n "$source" && -n "$fs_type" ]] || fail "cannot resolve the backing filesystem"
    [[ "$target" =~ ^/[A-Za-z0-9._/+:=-]*$ ]] || fail "backing mount target contains unsupported fstab characters"
    if [[ "$teardown_mode" != true ]]; then
        [[ ",$options," == *,rw,* ]] || fail "backing filesystem is not writable"
        if [[ "$fs_type" == ntfs3 && ",$options," == *,force,* ]]; then
            fail "refusing a forced NTFS3 mount; repair the filesystem and disable Windows hibernation first"
        fi
    fi
    if [[ -n "$backing_fs_uuid" ]]; then
        [[ "${actual_uuid,,}" == "${backing_fs_uuid,,}" ]] || fail "backing filesystem UUID is $actual_uuid, expected $backing_fs_uuid"
    fi
    case "$fs_type" in
        nfs*|cifs|smb*|fuse*|overlay|tmpfs)
            fail "unsupported backing filesystem type: $fs_type"
            ;;
    esac
    note "backing filesystem: $source ($fs_type, UUID ${actual_uuid:-unknown})"
}

check_outer_idle_timeout() {
    local parent target line options option value automount_unit load_state effective_timeout
    local -a option_list
    parent=$(dirname "$backing_file")
    while [[ ! -e "$parent" ]]; do
        parent=$(dirname "$parent")
    done
    target=$(findmnt -rn -T "$parent" -o TARGET | tail -n 1)
    line=$(awk -v target="$target" '$0 !~ /^[[:space:]]*#/ && $2 == target { print; exit }' /etc/fstab 2>/dev/null || true)
    if [[ -n "$line" ]]; then
        options=$(awk '{ print $4 }' <<<"$line")
        IFS=',' read -r -a option_list <<<"$options"
        for option in "${option_list[@]}"; do
            if [[ "$option" == x-systemd.idle-timeout=* ]]; then
                value=${option#*=}
                [[ "$value" == 0 || "$value" == 0s ]] || fail "$target has a finite systemd idle timeout; remove x-systemd.idle-timeout or set it to 0 in /etc/fstab first"
            fi
        done
    fi
    automount_unit=$(systemd-escape --path --suffix=automount "$target")
    load_state=$(systemctl show "$automount_unit" --property=LoadState --value 2>/dev/null || true)
    if [[ "$load_state" == loaded ]]; then
        effective_timeout=$(systemctl show "$automount_unit" --property=TimeoutIdleUSec --value 2>/dev/null || true)
        [[ "$effective_timeout" == 0 || "$effective_timeout" == infinity ]] || fail "$automount_unit still has effective idle timeout ${effective_timeout:-unknown}; run systemctl daemon-reload after fixing /etc/fstab"
    fi
}

check_key_autodiscovery() {
    local path
    for path in "/etc/cryptsetup-keys.d/$mapper_name.key" "/run/cryptsetup-keys.d/$mapper_name.key"; do
        [[ ! -e "$path" && ! -L "$path" ]] || fail "unexpected auto-discovered key file exists: $path"
    done
}

print_swap_warning() {
    local unsafe
    unsafe=$(awk 'NR > 1 && $1 !~ /^\/dev\/zram/ { print $1 }' /proc/swaps | paste -sd, -)
    if [[ -n "$unsafe" ]]; then
        note "warning: disk-backed swap is active ($unsafe); disable it or replace it with encrypted/zram swap before real-account login"
    fi
}

print_plan() {
    local requested_bytes
    requested_bytes=$((size_gib * GIB))
    note "backing file: $backing_file"
    note "fully allocated bytes: $requested_bytes"
    note "mapper: /dev/mapper/$mapper_name"
    note "mount point: $mount_point"
    note "owner: $owner_name"
    note "mount options: rw,nosuid,nodev,noatime,errors=remount-ro"
}

run_preflight() {
    local allow_idle_timeout="${1:-false}"
    validate_arguments
    require_commands
    check_backing_mount
    if [[ "$allow_idle_timeout" != true ]]; then
        check_outer_idle_timeout
    fi
    check_key_autodiscovery
    print_plan
    print_swap_warning
}

run_lock_preflight() {
    validate_arguments
    require_commands
    check_backing_mount true
    print_plan
    print_swap_warning
}

mapper_active() {
    cryptsetup status "$mapper_name" >/dev/null 2>&1
}

verify_backing_loop_set() {
    local candidate expected_loop expected_inode expected_device loop_records
    local loop_name loop_inode loop_device matching_count=0 matching_loop=""
    candidate="$1"
    expected_loop="${2:-}"
    expected_inode=$(stat -Lc '%i' -- "$candidate")
    expected_device=$(findmnt -rn -T "$candidate" -o MAJ:MIN | tail -n 1)
    [[ -n "$expected_device" ]] || fail "cannot resolve the backing file device"
    loop_records=$(losetup --list --noheadings --raw --output NAME,BACK-INO,BACK-MAJ:MIN) || fail "cannot enumerate loop devices for the backing file"
    while read -r loop_name loop_inode loop_device; do
        [[ -n "$loop_name" ]] || continue
        if [[ "$loop_inode" == "$expected_inode" && "$loop_device" == "$expected_device" ]]; then
            ((matching_count += 1))
            matching_loop="$loop_name"
        fi
    done <<<"$loop_records"

    if [[ -z "$expected_loop" ]]; then
        ((matching_count == 0)) || fail "backing file is already attached to a loop device while the expected mapper is inactive; close the unknown mapping manually"
        return
    fi
    ((matching_count == 1)) || fail "backing file is attached to $matching_count loop devices; refusing to continue while an additional mapping may remain open"
    [[ "$matching_loop" == "$expected_loop" ]] || fail "the only loop device for the backing file is not the expected mapper slave"
}

mapping_loop_device=""
mapping_dm_node=""

verify_mapping_identity() {
    local candidate mapper_path dm_node expected_uuid dm_uuid slave loop_record
    local loop_inode loop_device offset size_limit read_only expected_inode expected_device expected_size
    local -a slaves
    candidate="$1"
    mapping_loop_device=""
    mapping_dm_node=""
    mapper_path="/dev/mapper/$mapper_name"
    [[ -b "$mapper_path" && -f "$candidate" && ! -L "$candidate" ]] || fail "mapper or expected backing file is unavailable"
    dm_node=$(basename "$(readlink -f -- "$mapper_path")")
    [[ "$dm_node" == dm-* && -r "/sys/class/block/$dm_node/dm/uuid" ]] || fail "mapper is not a verifiable device-mapper device"

    expected_uuid=$(cryptsetup luksUUID "$candidate")
    expected_uuid=${expected_uuid//-/}
    dm_uuid=$(<"/sys/class/block/$dm_node/dm/uuid")
    [[ "${dm_uuid,,}" == crypt-luks2-"${expected_uuid,,}"-* ]] || fail "mapper LUKS UUID does not match the backing file"

    mapfile -t slaves < <(find "/sys/class/block/$dm_node/slaves" -mindepth 1 -maxdepth 1 -printf '%f\n')
    ((${#slaves[@]} == 1)) || fail "mapper must have exactly one loop-device slave"
    slave=${slaves[0]}
    [[ "$slave" == loop[0-9]* && -r "/sys/class/block/$slave/loop/backing_file" ]] || fail "mapper slave is not a loop device"
    loop_record=$(losetup --list --noheadings --raw --output BACK-INO,BACK-MAJ:MIN,OFFSET,SIZELIMIT,RO "/dev/$slave")
    read -r loop_inode loop_device offset size_limit read_only <<<"$loop_record"
    expected_inode=$(stat -Lc '%i' -- "$candidate")
    expected_device=$(findmnt -rn -T "$candidate" -o MAJ:MIN | tail -n 1)
    expected_size=$(stat -Lc '%s' -- "$candidate")
    [[ "$loop_inode" == "$expected_inode" && "$loop_device" == "$expected_device" ]] || fail "mapper loop device does not reference the expected backing file object"
    [[ "$offset" == 0 && "$read_only" == 0 ]] || fail "mapper loop device has unexpected offset or read-only mode"
    [[ "$size_limit" == 0 || "$size_limit" == "$expected_size" ]] || fail "mapper loop device has an unexpected size limit"
    mapping_loop_device="/dev/$slave"
    mapping_dm_node="$dm_node"
}

verify_mapping_holders() {
    local loop_device dm_node loop_node holder_dir holder_records holder
    local -a holders=()
    loop_device="$1"
    dm_node="$2"
    loop_node=$(basename "$loop_device")
    holder_dir="/sys/class/block/$loop_node/holders"
    [[ -d "$holder_dir" ]] || fail "cannot inspect holders for the mapper loop device"
    holder_records=$(find "$holder_dir" -mindepth 1 -maxdepth 1 -printf '%f\n') || fail "cannot enumerate holders for the mapper loop device"
    while IFS= read -r holder; do
        [[ -n "$holder" ]] && holders+=("$holder")
    done <<<"$holder_records"
    ((${#holders[@]} == 1)) || fail "mapper loop device has ${#holders[@]} holders; refusing while another mapper may expose the decrypted volume"
    [[ "${holders[0]}" == "$dm_node" ]] || fail "mapper loop device is held by an unexpected device-mapper device"
}

verify_mapping_backing() {
    local candidate
    candidate="$1"
    verify_mapping_identity "$candidate"
    verify_backing_loop_set "$candidate" "$mapping_loop_device"
    verify_mapping_holders "$mapping_loop_device" "$mapping_dm_node"
}

mount_source_matches() {
    local actual expected
    mountpoint -q "$mount_point" || return 1
    actual=$(findmnt -rn -M "$mount_point" -o SOURCE | tail -n 1)
    expected="/dev/mapper/$mapper_name"
    [[ -n "$actual" && "$(readlink -f "$actual")" == "$(readlink -f "$expected")" ]]
}

verify_backing_identity() {
	[[ -f "$backing_file" && ! -L "$backing_file" ]] || fail "encrypted backing file is missing or is not a regular file"
	[[ "$(stat -c '%h' "$backing_file")" -eq 1 ]] || fail "encrypted backing file must have exactly one hard link"
	cryptsetup isLuks --type luks2 "$backing_file" || fail "backing file is not LUKS2"
}

verify_backing_file() {
	local logical_bytes allocated_bytes expected_bytes
	verify_backing_identity
	logical_bytes=$(stat -c '%s' "$backing_file")
	allocated_bytes=$(( $(stat -c '%b' "$backing_file") * 512 ))
	expected_bytes=$((size_gib * GIB))
	[[ "$logical_bytes" -eq "$expected_bytes" ]] || fail "encrypted backing file size does not match --size-gib $size_gib"
	((allocated_bytes >= logical_bytes)) || fail "encrypted backing file is sparse or has been deallocated"
}

verify_mounted_volume() {
	local minimum_available marker marker_schema actual_fs_uuid marker_fs_uuid actual_luks_uuid marker_luks_uuid available_bytes mount_options
	minimum_available="${1:-0}"
	verify_mapping_backing "$backing_file"
	mount_source_matches || fail "the expected decrypted volume is not mounted"
	[[ "$(findmnt -rn -M "$mount_point" -o FSTYPE | tail -n 1)" == ext4 ]] || fail "mounted state filesystem is not ext4"
	mount_options=$(findmnt -rn -M "$mount_point" -o OPTIONS | tail -n 1)
	[[ ",$mount_options," == *,rw,* && ",$mount_options," == *,nosuid,* && ",$mount_options," == *,nodev,* ]] || fail "mounted state filesystem is missing required rw,nosuid,nodev options"
	marker="$mount_point/.wechatcopilot-volume"
	[[ -f "$marker" && ! -L "$marker" ]] || fail "mounted volume marker is missing or invalid"
	[[ "$(stat -c '%u:%a:%h' "$marker")" == "$(id -u -- "$owner_name"):600:1" ]] || fail "mounted volume marker has unsafe owner, mode, or hard-link count"
	marker_schema=$(awk -F= '$1 == "schema" { print $2 }' "$marker")
	marker_fs_uuid=$(awk -F= '$1 == "filesystem_uuid" { print $2 }' "$marker")
	marker_luks_uuid=$(awk -F= '$1 == "luks_uuid" { print $2 }' "$marker")
	actual_fs_uuid=$(blkid -s UUID -o value "/dev/mapper/$mapper_name")
	actual_luks_uuid=$(cryptsetup luksUUID "$backing_file")
	[[ "$marker_schema" == 1 ]] || fail "mounted volume marker schema is invalid"
	[[ -n "$marker_fs_uuid" && "${marker_fs_uuid,,}" == "${actual_fs_uuid,,}" ]] || fail "mounted filesystem UUID does not match its marker"
	[[ -n "$marker_luks_uuid" && "${marker_luks_uuid,,}" == "${actual_luks_uuid,,}" ]] || fail "LUKS UUID does not match the mounted volume marker"
	[[ "$(stat -c '%u:%a' "$mount_point")" == "$(id -u -- "$owner_name"):700" ]] || fail "mounted state root must be owned by $owner_name with mode 0700"
	if ((minimum_available > 0)); then
		available_bytes=$(df --output=avail -B1 "$mount_point" | tail -n 1 | tr -d ' ')
		((available_bytes >= minimum_available)) || fail "mounted state volume does not meet the required free-space reserve"
	fi
}

config_block_state() {
    local target body begin_count end_count current desired
    target="$1"
    body="$2"
    if [[ ! -e "$target" ]]; then
        printf 'absent\n'
        return
    fi
    begin_count=$(grep -Fxc "$CONFIG_BEGIN" "$target" || true)
    end_count=$(grep -Fxc "$CONFIG_END" "$target" || true)
    if [[ "$begin_count" == 0 && "$end_count" == 0 ]]; then
        printf 'absent\n'
        return
    fi
    [[ "$begin_count" == 1 && "$end_count" == 1 ]] || fail "$target contains duplicate or incomplete wechatcopilot markers"
    current=$(awk -v begin="$CONFIG_BEGIN" -v end="$CONFIG_END" '$0 == begin { copy=1 } copy { print } $0 == end { copy=0 }' "$target")
    desired=$(printf '%s\n%s\n%s' "$CONFIG_BEGIN" "$body" "$CONFIG_END")
    [[ "$current" == "$desired" ]] || fail "$target contains a different or malformed wechatcopilot block"
    printf 'exact\n'
}

config_file_snapshot() {
    local target metadata digest
    target="$1"
    if [[ ! -e "$target" && ! -L "$target" ]]; then
        printf 'absent\n'
        return
    fi
    [[ -f "$target" && ! -L "$target" ]] || return 1
    metadata=$(stat -Lc '%d:%i:%u:%g:%a:%s' -- "$target") || return 1
    digest=$(sha256sum -- "$target") || return 1
    digest=${digest%% *}
    printf 'file:%s:%s\n' "$metadata" "$digest"
}

config_file_matches_snapshot() {
    local actual
    actual=$(config_file_snapshot "$1") || return 1
    [[ "$actual" == "$2" ]]
}

check_config_collisions() {
    local target kind
    target="$1"
    kind="$2"
    [[ -e "$target" ]] || return
    if ! awk -v begin="$CONFIG_BEGIN" -v end="$CONFIG_END" -v kind="$kind" \
        -v mapper="$mapper_name" -v backing="$backing_file" -v mount="$mount_point" '
            $0 == begin { managed=1; next }
            $0 == end { managed=0; next }
            managed || $0 ~ /^[[:space:]]*#/ || NF == 0 { next }
            kind == "crypttab" && ($1 == mapper || $2 == backing) { exit 1 }
            kind == "fstab" && ($1 == "/dev/mapper/" mapper || $2 == mount) { exit 1 }
        ' "$target"; then
        fail "$target contains an unmanaged entry that collides with this state volume"
    fi
}

prepare_config_file() {
    local target mode body state temporary
    target="$1"
    mode="$2"
    body="$3"
    state="$4"
    temporary=$(mktemp "$(dirname "$target")/.wechatcopilot-config.XXXXXX")
    if [[ -e "$target" ]]; then
        cat "$target" >"$temporary"
    fi
    if [[ "$state" == absent ]]; then
        [[ ! -s "$temporary" ]] || printf '\n' >>"$temporary"
        printf '%s\n%s\n%s\n' "$CONFIG_BEGIN" "$body" "$CONFIG_END" >>"$temporary"
    fi
    chown root:root "$temporary"
    chmod "$mode" "$temporary"
    sync -f "$temporary"
    printf '%s\n' "$temporary"
}

backup_config_file() {
    local target backup
    target="$1"
    if [[ ! -e "$target" ]]; then
        printf 'missing\n'
        return
    fi
    backup=$(mktemp "$target.wechatcopilot-backup.XXXXXX")
    cp -a -- "$target" "$backup"
    chown root:root "$backup"
    chmod 0600 "$backup"
    sync -f "$backup"
    printf '%s\n' "$backup"
}

restore_config_file() {
    local target backup mode temporary
    target="$1"
    backup="$2"
    mode="$3"
    if [[ "$backup" == missing ]]; then
        rm -f -- "$target"
        return
    fi
    temporary=$(mktemp "$(dirname "$target")/.wechatcopilot-restore.XXXXXX")
    cat "$backup" >"$temporary"
    chown root:root "$temporary"
    chmod "$mode" "$temporary"
    sync -f "$temporary"
    mv -fT -- "$temporary" "$target"
}

verify_generated_units() {
    local crypt_unit mount_unit load_state source_path fragment_path
    crypt_unit=$(systemd-escape --template=systemd-cryptsetup@.service "$mapper_name")
    mount_unit=$(systemd-escape --path --suffix=mount "$mount_point")
    systemctl daemon-reload || return 1
    load_state=$(systemctl show "$crypt_unit" --property=LoadState --value) || return 1
    [[ "$load_state" == loaded ]] || return 1
    source_path=$(systemctl show "$crypt_unit" --property=SourcePath --value) || return 1
    [[ "$source_path" == /etc/crypttab ]] || return 1
    fragment_path=$(systemctl show "$crypt_unit" --property=FragmentPath --value) || return 1
    [[ "$fragment_path" == "/run/systemd/generator/$crypt_unit" ]] || return 1
    load_state=$(systemctl show "$mount_unit" --property=LoadState --value) || return 1
    [[ "$load_state" == loaded ]] || return 1
    source_path=$(systemctl show "$mount_unit" --property=SourcePath --value) || return 1
    [[ "$source_path" == /etc/fstab ]] || return 1
    fragment_path=$(systemctl show "$mount_unit" --property=FragmentPath --value) || return 1
    [[ "$fragment_path" == "/run/systemd/generator/$mount_unit" ]] || return 1
}

verify_system_config() {
    local crypt_line mount_line crypt_state fstab_state
    crypt_line="$mapper_name $backing_file none luks,noauto,nofail,timeout=120s,tries=3,password-cache=no"
    mount_line="/dev/mapper/$mapper_name $mount_point ext4 rw,nosuid,nodev,noatime,errors=remount-ro,noauto,nofail,x-systemd.requires-mounts-for=$(dirname "$backing_file"),x-systemd.device-timeout=150s 0 2"
    crypt_state=$(config_block_state /etc/crypttab "$crypt_line")
    fstab_state=$(config_block_state /etc/fstab "$mount_line")
    [[ "$crypt_state" == exact && "$fstab_state" == exact ]] || fail "state-volume system configuration is missing; run configure first"
    check_config_collisions /etc/crypttab crypttab
    check_config_collisions /etc/fstab fstab
    verify_generated_units || fail "generated state-volume units did not validate"
}

install_system_config() {
    local crypt_line mount_line crypt_state fstab_state crypt_candidate fstab_candidate
    local crypt_backup fstab_backup crypt_original_snapshot fstab_original_snapshot
    local crypt_candidate_snapshot fstab_candidate_snapshot
    local rollback_needed=false crypt_replaced=false fstab_replaced=false rollback_incomplete=false
    local saved_hup saved_int saved_term
    crypt_line="$mapper_name $backing_file none luks,noauto,nofail,timeout=120s,tries=3,password-cache=no"
    mount_line="/dev/mapper/$mapper_name $mount_point ext4 rw,nosuid,nodev,noatime,errors=remount-ro,noauto,nofail,x-systemd.requires-mounts-for=$(dirname "$backing_file"),x-systemd.device-timeout=150s 0 2"
    [[ ! -L /etc/crypttab && ! -L /etc/fstab ]] || fail "crypttab and fstab must not be symlinks"
    crypt_original_snapshot=$(config_file_snapshot /etc/crypttab) || fail "cannot snapshot /etc/crypttab"
    fstab_original_snapshot=$(config_file_snapshot /etc/fstab) || fail "cannot snapshot /etc/fstab"
    crypt_state=$(config_block_state /etc/crypttab "$crypt_line")
    fstab_state=$(config_block_state /etc/fstab "$mount_line")
    check_config_collisions /etc/crypttab crypttab
    check_config_collisions /etc/fstab fstab
    if [[ "$crypt_state" == exact && "$fstab_state" == exact ]]; then
        config_file_matches_snapshot /etc/crypttab "$crypt_original_snapshot" || fail "/etc/crypttab changed while it was being validated; retry after the other writer finishes"
        config_file_matches_snapshot /etc/fstab "$fstab_original_snapshot" || fail "/etc/fstab changed while it was being validated; retry after the other writer finishes"
        verify_generated_units || fail "generated state-volume units did not validate"
        return
    fi

    crypt_candidate=$(prepare_config_file /etc/crypttab 0600 "$crypt_line" "$crypt_state")
    fstab_candidate=$(prepare_config_file /etc/fstab 0644 "$mount_line" "$fstab_state")
    findmnt --verify --verbose --tab-file "$fstab_candidate" >/dev/null
    crypt_backup=$(backup_config_file /etc/crypttab)
    fstab_backup=$(backup_config_file /etc/fstab)
    if [[ "$crypt_original_snapshot" == absent ]]; then
        [[ "$crypt_backup" == missing ]] || fail "crypttab backup state does not match the original"
    else
        [[ "$crypt_backup" != missing ]] && cmp -s -- /etc/crypttab "$crypt_backup" || fail "crypttab changed while its backup was created"
    fi
    if [[ "$fstab_original_snapshot" == absent ]]; then
        [[ "$fstab_backup" == missing ]] || fail "fstab backup state does not match the original"
    else
        [[ "$fstab_backup" != missing ]] && cmp -s -- /etc/fstab "$fstab_backup" || fail "fstab changed while its backup was created"
    fi
    crypt_candidate_snapshot=$(config_file_snapshot "$crypt_candidate") || fail "cannot snapshot staged crypttab"
    fstab_candidate_snapshot=$(config_file_snapshot "$fstab_candidate") || fail "cannot snapshot staged fstab"
    config_file_matches_snapshot /etc/crypttab "$crypt_original_snapshot" || fail "/etc/crypttab changed while the update was being prepared; no configuration was replaced"
    config_file_matches_snapshot /etc/fstab "$fstab_original_snapshot" || fail "/etc/fstab changed while the update was being prepared; no configuration was replaced"

    rollback_system_config() {
        if [[ "$rollback_needed" == true ]]; then
            set +e
            rollback_incomplete=false
            if [[ "$fstab_replaced" == true ]]; then
                if config_file_matches_snapshot /etc/fstab "$fstab_candidate_snapshot"; then
                    restore_config_file /etc/fstab "$fstab_backup" 0644 || rollback_incomplete=true
                else
                    printf 'warning: /etc/fstab changed after installation began; refusing to overwrite the concurrent edit during rollback\n' >&2
                    rollback_incomplete=true
                fi
            fi
            if [[ "$crypt_replaced" == true ]]; then
                if config_file_matches_snapshot /etc/crypttab "$crypt_candidate_snapshot"; then
                    restore_config_file /etc/crypttab "$crypt_backup" 0600 || rollback_incomplete=true
                else
                    printf 'warning: /etc/crypttab changed after installation began; refusing to overwrite the concurrent edit during rollback\n' >&2
                    rollback_incomplete=true
                fi
            fi
            sync -f /etc || rollback_incomplete=true
            systemctl daemon-reload >/dev/null 2>&1 || rollback_incomplete=true
            [[ "$rollback_incomplete" == false ]] || printf 'warning: system configuration rollback was incomplete; inspect /etc/crypttab and /etc/fstab manually\n' >&2
            rollback_needed=false
        fi
    }
    saved_hup=$(trap -p HUP || true)
    saved_int=$(trap -p INT || true)
    saved_term=$(trap -p TERM || true)
    rollback_needed=true
    trap 'rollback_system_config; exit 129' HUP
    trap 'rollback_system_config; exit 130' INT
    trap 'rollback_system_config; exit 143' TERM

    if ! config_file_matches_snapshot /etc/crypttab "$crypt_original_snapshot" || ! config_file_matches_snapshot /etc/fstab "$fstab_original_snapshot"; then
        rollback_system_config
        fail "system configuration changed before installation; no configuration was replaced"
    fi
    if [[ "$crypt_original_snapshot" == absent ]]; then
        if ! mv -Tn -- "$crypt_candidate" /etc/crypttab; then
            rollback_system_config
            fail "cannot atomically install crypttab"
        fi
        if [[ -e "$crypt_candidate" || -L "$crypt_candidate" ]]; then
            rollback_system_config
            fail "/etc/crypttab appeared concurrently; refusing to overwrite it"
        fi
    elif ! mv -fT -- "$crypt_candidate" /etc/crypttab; then
        rollback_system_config
        fail "cannot atomically install crypttab"
    fi
    crypt_replaced=true
    if ! config_file_matches_snapshot /etc/crypttab "$crypt_candidate_snapshot" || ! config_file_matches_snapshot /etc/fstab "$fstab_original_snapshot"; then
        rollback_system_config
        fail "system configuration changed while installing crypttab"
    fi
    if [[ "$fstab_original_snapshot" == absent ]]; then
        if ! mv -Tn -- "$fstab_candidate" /etc/fstab; then
            rollback_system_config
            fail "cannot atomically install fstab"
        fi
        if [[ -e "$fstab_candidate" || -L "$fstab_candidate" ]]; then
            rollback_system_config
            fail "/etc/fstab appeared concurrently; refusing to overwrite it"
        fi
    elif ! mv -fT -- "$fstab_candidate" /etc/fstab; then
        rollback_system_config
        fail "cannot atomically install fstab"
    fi
    fstab_replaced=true
    if ! config_file_matches_snapshot /etc/crypttab "$crypt_candidate_snapshot" || ! config_file_matches_snapshot /etc/fstab "$fstab_candidate_snapshot"; then
        rollback_system_config
        fail "system configuration changed before it could be verified"
    fi
    if ! sync -f /etc; then
        rollback_system_config
        fail "cannot sync system configuration"
    fi
    if ! (verify_generated_units); then
        rollback_system_config
        fail "generated state-volume units did not validate"
    fi

    rollback_needed=false
    trap - HUP INT TERM
    [[ -z "$saved_hup" ]] || eval "$saved_hup"
    [[ -z "$saved_int" ]] || eval "$saved_int"
    [[ -z "$saved_term" ]] || eval "$saved_term"
    note "configuration backups: $crypt_backup $fstab_backup"
}

start_volume_units() {
    local crypt_unit mount_unit attempt load_state
    local mapper_was_active=false cleanup_needed=false
    crypt_unit=$(systemd-escape --template=systemd-cryptsetup@.service "$mapper_name")
    mount_unit=$(systemd-escape --path --suffix=mount "$mount_point")
    verify_system_config
    if mapper_active; then
        mapper_was_active=true
        verify_mapping_backing "$backing_file"
    else
        verify_backing_loop_set "$backing_file"
    fi
    if mountpoint -q "$mount_point"; then
        verify_mounted_volume
        systemctl start "$crypt_unit"
        systemctl is-active --quiet "$crypt_unit" || fail "systemd did not claim the existing verified mapper"
        systemctl start "$mount_unit"
        systemctl is-active --quiet "$mount_unit" || fail "systemd did not claim the existing verified mount"
        note "state volume is already unlocked and mounted at $mount_point"
        return
    fi

    cleanup_started_volume() {
        if [[ "$cleanup_needed" == true ]]; then
            set +e
            if ! mountpoint -q "$mount_point" || { mount_source_matches && (verify_mapping_identity "$backing_file") >/dev/null 2>&1; }; then
                systemctl stop "$mount_unit" >/dev/null 2>&1
            fi
            if [[ "$mapper_was_active" == false ]] && mapper_active && (verify_mapping_identity "$backing_file") >/dev/null 2>&1; then
                if systemctl is-active --quiet "$crypt_unit"; then
                    systemctl stop "$crypt_unit" >/dev/null 2>&1
                else
                    cryptsetup close "$mapper_name" >/dev/null 2>&1
                fi
            fi
            systemctl reset-failed "$crypt_unit" "$mount_unit" >/dev/null 2>&1
            mountpoint -q "$mount_point" && printf 'warning: failed unlock cleanup left the state mount active: %s\n' "$mount_point" >&2
            if [[ "$mapper_was_active" == false ]] && mapper_active; then
                printf 'warning: failed unlock cleanup left the project mapper active: %s\n' "/dev/mapper/$mapper_name" >&2
            fi
            if [[ "$mapper_was_active" == false ]] && ! (verify_backing_loop_set "$backing_file") >/dev/null 2>&1; then
                printf 'warning: failed unlock cleanup left the encrypted backing file attached to a loop device; inspect unknown mappings manually\n' >&2
            fi
        fi
    }
    cleanup_needed=true
    trap cleanup_started_volume EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM

    load_state=$(systemctl show "$crypt_unit" --property=LoadState --value)
    [[ "$load_state" == loaded ]] || fail "$crypt_unit is not loaded; run configure first"
    load_state=$(systemctl show "$mount_unit" --property=LoadState --value)
    [[ "$load_state" == loaded ]] || fail "$mount_unit is not loaded; run configure first"
    systemctl reset-failed "$crypt_unit" "$mount_unit" >/dev/null 2>&1 || true
    if [[ "$mapper_was_active" == true ]]; then
        systemctl start "$crypt_unit"
        systemctl is-active --quiet "$crypt_unit" || fail "systemd did not claim the existing verified mapper"
    fi
    systemctl start --no-block "$mount_unit"
    for ((attempt = 0; attempt < 600; attempt++)); do
        systemd-tty-ask-password-agent --query || true
        if systemctl is-active --quiet "$mount_unit"; then
            break
        fi
        systemctl is-failed --quiet "$crypt_unit" && fail "cryptsetup unit failed"
        systemctl is-failed --quiet "$mount_unit" && fail "mount unit failed"
        sleep 0.2
    done
    systemctl is-active --quiet "$mount_unit" || fail "timed out mounting the state volume"
    systemctl is-active --quiet "$crypt_unit" || fail "state mapper is not managed by systemd"
    verify_mounted_volume

    cleanup_needed=false
    trap - EXIT HUP INT TERM
    note "state volume is unlocked and mounted at $mount_point"
}

configure_volume() {
	local fs_uuid
	validate_arguments
	require_root
	acquire_operation_lock
	require_commands
	require_backing_uuid
	run_preflight
	verify_backing_file
	verify_mounted_volume
	fs_uuid=$(blkid -s UUID -o value "/dev/mapper/$mapper_name")
	install_system_config
	start_volume_units
	note "system configuration is installed"
	note "WECHATCOPILOT_HOME=$mount_point"
	note "WECHATCOPILOT_STATE_MOUNT_SOURCE=/dev/mapper/$mapper_name"
	note "WECHATCOPILOT_STATE_MOUNT_FSTYPE=ext4"
	note "WECHATCOPILOT_STATE_MOUNT_UUID=$fs_uuid"
}

create_volume() {
    local backing_dir staging requested_bytes available_bytes allocated_bytes logical_bytes
    local owner_group fs_uuid luks_uuid opened_backing
    local formatted=false mounted=false opened_by_us=false cleanup_needed=false
    validate_arguments
	require_root
	acquire_operation_lock
	require_tty
    require_commands
    [[ "$confirm_create" == true ]] || fail "create requires --confirm-create"
    require_backing_uuid
    run_preflight
    [[ ! -e "$backing_file" ]] || fail "backing file already exists; it will never be overwritten"
    [[ ! -e "/dev/mapper/$mapper_name" && ! -L "/dev/mapper/$mapper_name" ]] || fail "mapper already exists: $mapper_name"
    [[ ! -e "$mount_point" ]] || {
        [[ -d "$mount_point" && ! -L "$mount_point" ]] || fail "mount point exists and is not a directory"
        [[ -z "$(find "$mount_point" -mindepth 1 -maxdepth 1 -print -quit)" ]] || fail "mount point is not empty"
        mountpoint -q "$mount_point" && fail "mount point is already mounted"
    }

    backing_dir=$(dirname "$backing_file")
    install -d -m 0700 "$backing_dir"
    requested_bytes=$((size_gib * GIB))
    available_bytes=$(df --output=avail -B1 "$backing_dir" | tail -n 1 | tr -d ' ')
    ((available_bytes >= requested_bytes + 5 * GIB)) || fail "backing filesystem needs the requested size plus 5 GiB headroom"
    staging=$(mktemp "$backing_dir/.state.luks.creating.XXXXXX")
    opened_backing="$staging"

    mapping_matches_created_backing() {
        local candidate
        for candidate in "$opened_backing" "$staging" "$backing_file"; do
            if [[ -f "$candidate" ]] && (verify_mapping_identity "$candidate") >/dev/null 2>&1; then
                return 0
            fi
        done
        return 1
    }

    created_backing_attachment_remains() {
        local candidate
        for candidate in "$opened_backing" "$staging" "$backing_file"; do
            if [[ -f "$candidate" ]] && ! (verify_backing_loop_set "$candidate") >/dev/null 2>&1; then
                return 0
            fi
        done
        return 1
    }

    cleanup_create() {
        [[ "$cleanup_needed" == true ]] || return
        set +e
        if [[ "$mounted" == true && "$opened_by_us" == true ]] && mount_source_matches && mapping_matches_created_backing; then
            umount "$mount_point"
        fi
        if [[ "$opened_by_us" == true ]] && mapper_active && mapping_matches_created_backing; then
            cryptsetup close "$mapper_name"
        fi
        mountpoint -q "$mount_point" && printf 'warning: failed create cleanup left the state mount active: %s\n' "$mount_point" >&2
        if [[ "$opened_by_us" == true ]] && mapper_active; then
            printf 'warning: failed create cleanup left the project mapper active: %s\n' "/dev/mapper/$mapper_name" >&2
        fi
        if [[ "$opened_by_us" == true ]] && created_backing_attachment_remains; then
            printf 'warning: failed create cleanup left the encrypted backing file attached to a loop device; inspect unknown mappings manually\n' >&2
        fi
        if [[ "$formatted" == false && -e "$staging" ]]; then
            rm -f -- "$staging"
        elif [[ -e "$staging" ]]; then
            printf 'retained encrypted staging image for diagnosis: %s\n' "$staging" >&2
        fi
    }
    cleanup_needed=true
    trap cleanup_create EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM

    fallocate --length "$requested_bytes" "$staging"
    chmod 0600 "$staging"
    logical_bytes=$(stat -c '%s' "$staging")
    allocated_bytes=$(( $(stat -c '%b' "$staging") * 512 ))
    [[ "$logical_bytes" -eq "$requested_bytes" && "$allocated_bytes" -ge "$logical_bytes" ]] || fail "backing image is sparse or incompletely allocated"
    sync -f "$staging"

    note "cryptsetup will now request confirmation and a new recovery passphrase directly on this terminal."
    cryptsetup luksFormat --type luks2 --verify-passphrase "$staging"
    formatted=true
    cryptsetup open --type luks2 "$staging" "$mapper_name"
    opened_by_us=true
    verify_mapping_backing "$staging"
    [[ -z "$(blkid -p -o value -s TYPE "/dev/mapper/$mapper_name" 2>/dev/null || true)" ]] || fail "new mapper unexpectedly contains a filesystem signature"
    mkfs.ext4 -E nodiscard -L wcp-state -m 1 "/dev/mapper/$mapper_name"
    allocated_bytes=$(( $(stat -c '%b' "$staging") * 512 ))
    ((allocated_bytes >= logical_bytes)) || fail "filesystem creation made the backing image sparse"

    install -d -o root -g root -m 0700 "$mount_point"
    [[ "$(stat -c '%u:%a' "$mount_point")" == "0:700" ]] || fail "unmounted state mountpoint must be root-owned with mode 0700"
    mount -t ext4 -o rw,nosuid,nodev,noatime,errors=remount-ro "/dev/mapper/$mapper_name" "$mount_point"
    mounted=true
    owner_group=$(id -gn -- "$owner_name")
    chown "$owner_name:$owner_group" "$mount_point"
    chmod 0700 "$mount_point"
    fs_uuid=$(blkid -s UUID -o value "/dev/mapper/$mapper_name")
    luks_uuid=$(cryptsetup luksUUID "$staging")
    install -o "$owner_name" -g "$owner_group" -m 0600 /dev/null "$mount_point/.wechatcopilot-volume"
    printf 'schema=1\nfilesystem_uuid=%s\nluks_uuid=%s\n' "$fs_uuid" "$luks_uuid" >"$mount_point/.wechatcopilot-volume"
    chown "$owner_name:$owner_group" "$mount_point/.wechatcopilot-volume"
	chmod 0600 "$mount_point/.wechatcopilot-volume"
	sync -f "$mount_point"

	mv -Tn -- "$staging" "$backing_file"
	[[ ! -e "$staging" && -f "$backing_file" ]] || fail "backing destination appeared during creation; the encrypted staging image was retained"
	opened_backing="$backing_file"
	sync -f "$backing_dir"
	verify_backing_file
	verify_mounted_volume $((30 * GIB))
	install_system_config

    sync -f "$mount_point"
    umount "$mount_point"
    mounted=false
    verify_mapping_backing "$backing_file"
    cryptsetup close "$mapper_name"
    opened_by_us=false
    cleanup_needed=false
    trap - EXIT HUP INT TERM

    note "systemd will now request the passphrase once to verify the persistent unlock path."
    start_volume_units
    verify_mounted_volume $((30 * GIB))

    note "state volume created and mounted successfully"
    note "add these non-secret values to the daemon environment file:"
    note "WECHATCOPILOT_HOME=$mount_point"
    note "WECHATCOPILOT_STATE_MOUNT_SOURCE=/dev/mapper/$mapper_name"
    note "WECHATCOPILOT_STATE_MOUNT_FSTYPE=ext4"
    note "WECHATCOPILOT_STATE_MOUNT_UUID=$fs_uuid"
    print_swap_warning
}

unlock_volume() {
    validate_arguments
	require_root
	acquire_operation_lock
	require_tty
    require_commands
    require_backing_uuid
    run_preflight
	verify_backing_file
    start_volume_units
}

lock_volume() {
    local crypt_unit mount_unit running user_id user_runtime user_state
    validate_arguments
	require_root
	acquire_operation_lock
	require_commands
    require_backing_uuid
    [[ "$confirm_daemon_stopped" == true ]] || fail "lock requires --confirm-daemon-stopped"
	run_lock_preflight
	verify_backing_identity
    if mountpoint -q "$mount_point"; then
        verify_mapping_backing "$backing_file"
        mount_source_matches || fail "refusing to unmount an unexpected source"
    elif mapper_active; then
        verify_mapping_backing "$backing_file"
    else
        verify_backing_loop_set "$backing_file"
        note "state volume is already locked"
        return
    fi

	user_id=$(id -u -- "$owner_name")
	user_runtime="/run/user/$user_id"
	if [[ -S "$user_runtime/bus" ]]; then
		command -v runuser >/dev/null || fail "runuser is required to verify the user daemon"
		runuser -u "$owner_name" -- env XDG_RUNTIME_DIR="$user_runtime" DBUS_SESSION_BUS_ADDRESS="unix:path=$user_runtime/bus" systemctl --user show-environment >/dev/null 2>&1 || fail "cannot connect to the user service manager"
		user_state=$(runuser -u "$owner_name" -- env XDG_RUNTIME_DIR="$user_runtime" DBUS_SESSION_BUS_ADDRESS="unix:path=$user_runtime/bus" systemctl --user is-active wechatcopilot.service 2>/dev/null || true)
		[[ "$user_state" != active && "$user_state" != activating && "$user_state" != reloading ]] || fail "wechatcopilot user daemon is still $user_state"
	fi
	command -v docker >/dev/null || fail "docker is required to verify that client containers are stopped"
	docker info >/dev/null 2>&1 || fail "cannot verify Docker container state"
	running=$(docker ps -q --filter label=io.wechatcopilot.driver)
	running+=$(docker ps -q --filter label=dev.wechatcopilot.driver)
	[[ -z "$running" ]] || fail "wechatcopilot containers are still running"
    crypt_unit=$(systemd-escape --template=systemd-cryptsetup@.service "$mapper_name")
    mount_unit=$(systemd-escape --path --suffix=mount "$mount_point")
    if mountpoint -q "$mount_point"; then
        verify_mapping_backing "$backing_file"
        mount_source_matches || fail "refusing to unmount an unexpected source"
        sync -f "$mount_point" || note "warning: state filesystem sync reported an error; attempting a normal unmount"
        systemctl stop "$mount_unit"
        mountpoint -q "$mount_point" && fail "state mount is still active; stop the daemon and all processes using it"
    fi
	if mapper_active; then
		verify_mapping_backing "$backing_file"
		if systemctl is-active --quiet "$crypt_unit"; then
			systemctl stop "$crypt_unit"
		else
			cryptsetup close "$mapper_name"
		fi
    fi
    mountpoint -q "$mount_point" && fail "state mount is still active"
    mapper_active && fail "state mapper is still active"
    verify_backing_loop_set "$backing_file"
    note "state volume is locked"
}

show_status() {
    validate_arguments
    note "backing_present=$([[ -f "$backing_file" && ! -L "$backing_file" ]] && printf yes || printf no)"
    note "mapper_active=$(mapper_active && printf yes || printf no)"
    note "mount_active=$(mountpoint -q "$mount_point" && printf yes || printf no)"
    if [[ -f "$backing_file" ]] && mapper_active; then
        if (verify_mapping_backing "$backing_file") >/dev/null 2>&1; then
            note "mapper_matches_backing=yes"
        else
            note "mapper_matches_backing=no"
        fi
    fi
    if mountpoint -q "$mount_point"; then
        findmnt -rn -M "$mount_point" -o TARGET,SOURCE,FSTYPE,OPTIONS
    fi
    print_swap_warning
}

case "$command_name" in
    preflight)
        run_preflight
        ;;
	create)
		create_volume
		;;
	configure)
		configure_volume
		;;
    unlock)
        unlock_volume
        ;;
    lock)
        lock_volume
        ;;
    status)
        show_status
        ;;
	help|-h|--help)
        usage
        ;;
    *)
        usage >&2
        fail "unknown command: $command_name"
        ;;
esac
