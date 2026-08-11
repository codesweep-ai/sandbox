// initramfs-init — the microVM's first userspace process.
//
// This replaces a ~38 MiB dracut initrd whose generality buys nothing here: a
// cs-sandbox microVM always boots exactly one ext4 root behind virtio-mmio, on a
// device list fixed at create time. Dracut spends ~2.4 s probing for hardware,
// storage stacks and network setups that cannot exist in this VM.
//
// Something still has to run first, though: Fedora builds virtio_mmio as a
// module (CONFIG_VIRTIO_MMIO=m), so no block device exists until it is loaded —
// which is why the kernel cannot simply mount root=/dev/vda on its own. Loading
// that module, mounting root and handing over to the real guest init is this
// program's whole job.
//
// Built static and packed into initrd.img by internal/fcdisk (buildFedoraKernel).

#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#define NEWROOT "/newroot"

// die reports on the serial console before exiting. The kernel panics the moment
// init exits, and that panic message alone would say nothing about the cause —
// on a failed boot this line is the only diagnostic anyone gets.
static void die(const char *what) {
    fprintf(stderr, "\ninitramfs-init: %s failed (errno=%d: %s)\n",
            what, errno, strerror(errno));
    fflush(stderr);
    sleep(1);
    _exit(1);
}

// cmdline_get copies the value of key= from /proc/cmdline into out.
// Returns 1 when found. Bare flags (e.g. "ro") yield an empty value.
static int cmdline_get(const char *key, char *out, size_t outsz) {
    static char buf[4096];
    static int loaded = 0;
    if (!loaded) {
        int fd = open("/proc/cmdline", O_RDONLY);
        if (fd < 0) return 0;
        ssize_t n = read(fd, buf, sizeof(buf) - 1);
        close(fd);
        if (n < 0) n = 0;
        buf[n] = '\0';
        loaded = 1;
    }
    size_t klen = strlen(key);
    for (char *p = buf; *p;) {
        while (*p == ' ' || *p == '\n') p++;
        char *end = p;
        while (*end && *end != ' ' && *end != '\n') end++;
        if ((size_t)(end - p) >= klen && strncmp(p, key, klen) == 0) {
            if (p[klen] == '=') {
                size_t vlen = (size_t)(end - p) - klen - 1;
                if (vlen >= outsz) vlen = outsz - 1;
                memcpy(out, p + klen + 1, vlen);
                out[vlen] = '\0';
                return 1;
            }
            if (p[klen] == ' ' || p[klen] == '\n' || p[klen] == '\0') {
                out[0] = '\0';
                return 1;
            }
        }
        p = end;
    }
    return 0;
}

// load_modules insmods every *.ko in /modules in lexical order. The build names
// them with a numeric prefix so load order is explicit; a module that is built
// into this kernel simply fails with EEXIST and is skipped.
static void load_modules(void) {
    struct dirent **names;
    int n = scandir("/modules", &names, NULL, alphasort);
    if (n < 0) return;
    for (int i = 0; i < n; i++) {
        const char *nm = names[i]->d_name;
        size_t len = strlen(nm);
        if (len > 3 && strcmp(nm + len - 3, ".ko") == 0) {
            char path[512];
            snprintf(path, sizeof(path), "/modules/%s", nm);
            int fd = open(path, O_RDONLY | O_CLOEXEC);
            if (fd >= 0) {
                if (syscall(SYS_finit_module, fd, "", 0) != 0 && errno != EEXIST) {
                    fprintf(stderr, "initramfs-init: insmod %s: %s\n", nm, strerror(errno));
                }
                close(fd);
            }
        }
        free(names[i]);
    }
    free(names);
}

// wait_for_device polls for the root block device. virtio_mmio probes its
// devices during module init and devtmpfs creates the node synchronously, so
// this normally succeeds on the first look; the loop only covers a slow probe.
static int wait_for_device(const char *dev, int timeout_ms) {
    struct stat st;
    for (int waited = 0; waited <= timeout_ms; waited += 10) {
        if (stat(dev, &st) == 0) return 1;
        struct timespec ts = {0, 10 * 1000 * 1000};
        nanosleep(&ts, NULL);
    }
    return 0;
}

int main(void) {
    char root[256] = "/dev/vda";
    char init[256] = "/fc-init";
    char flag[64];

    if (mount("proc", "/proc", "proc", 0, NULL) != 0) die("mount /proc");
    if (mount("sysfs", "/sys", "sysfs", 0, NULL) != 0) die("mount /sys");
    if (mount("devtmpfs", "/dev", "devtmpfs", 0, NULL) != 0) die("mount /dev");

    cmdline_get("root", root, sizeof(root));
    cmdline_get("init", init, sizeof(init));

    load_modules();

    if (!wait_for_device(root, 5000)) {
        fprintf(stderr, "initramfs-init: %s never appeared\n", root);
        die("wait for root device");
    }

    // `ro` on the cmdline wins; otherwise the root is mounted read-write, which
    // is what cs-sandbox's guest init expects.
    unsigned long mflags = 0;
    if (cmdline_get("ro", flag, sizeof(flag))) mflags |= MS_RDONLY;
    if (mount(root, NEWROOT, "ext4", mflags, NULL) != 0) die("mount root");

    // Hand the pseudo-filesystems to the new root rather than unmounting them:
    // MS_MOVE keeps them mounted across the switch, so the guest init inherits a
    // working /proc, /sys and /dev instead of racing to recreate them.
    if (mount("/dev", NEWROOT "/dev", NULL, MS_MOVE, NULL) != 0) die("move /dev");
    if (mount("/sys", NEWROOT "/sys", NULL, MS_MOVE, NULL) != 0) die("move /sys");
    if (mount("/proc", NEWROOT "/proc", NULL, MS_MOVE, NULL) != 0) die("move /proc");

    // switch_root: make the new root the filesystem root, then exec the real
    // init. The initramfs itself is left behind as unreferenced rootfs pages —
    // ~1 MiB, versus the ~38 MiB dracut occupied.
    if (chdir(NEWROOT) != 0) die("chdir newroot");
    if (mount(".", "/", NULL, MS_MOVE, NULL) != 0) die("move newroot to /");
    if (chroot(".") != 0) die("chroot");
    if (chdir("/") != 0) die("chdir /");

    char *argv[] = {init, NULL};
    execv(init, argv);
    die("exec init");
    return 1;
}
