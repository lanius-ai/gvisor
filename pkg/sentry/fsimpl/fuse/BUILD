load("//pkg/sync/locking:locking.bzl", "declare_mutex")
load("//tools:defs.bzl", "go_library", "go_test")
load("//tools/go_generics:defs.bzl", "go_template_instance")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_template_instance(
    name = "request_list",
    out = "request_list.go",
    package = "fuse",
    prefix = "request",
    template = "//pkg/ilist:generic_list",
    types = {
        "Element": "*Request",
        "Linker": "*Request",
    },
)

go_template_instance(
    name = "inode_refs",
    out = "inode_refs.go",
    package = "fuse",
    prefix = "inode",
    template = "//pkg/refs:refs_template",
    types = {
        "T": "inode",
    },
)

go_template_instance(
    name = "connection_refs",
    out = "connection_refs.go",
    package = "fuse",
    prefix = "connection",
    template = "//pkg/refs:refs_template",
    types = {
        "T": "connection",
    },
)

go_template_instance(
    name = "seqatomic_time",
    out = "seqatomic_time_unsafe.go",
    imports = {
        "ktime": "gvisor.dev/gvisor/pkg/sentry/ktime",
    },
    package = "fuse",
    suffix = "Time",
    template = "//pkg/sync/seqatomic:generic_seqatomic",
    types = {
        "Value": "ktime.Time",
    },
)

declare_mutex(
    name = "inode_attr_mutex",
    out = "inode_attr_mutex.go",
    package = "fuse",
    prefix = "inodeAttr",
)

declare_mutex(
    name = "inode_data_mutex",
    out = "inode_data_mutex.go",
    package = "fuse",
    prefix = "inodeData",
)

declare_mutex(
    name = "inode_io_mutex",
    out = "inode_io_mutex.go",
    package = "fuse",
    prefix = "inodeIO",
)

declare_mutex(
    name = "inode_maps_mutex",
    out = "inode_maps_mutex.go",
    package = "fuse",
    prefix = "inodeMaps",
)

go_library(
    name = "fuse",
    srcs = [
        "connection.go",
        "connection_control.go",
        "connection_refs.go",
        "dev.go",
        "directory.go",
        "file.go",
        "fusefs.go",
        "host_connection.go",
        "inode.go",
        "inode_attr_mutex.go",
        "inode_connection.go",
        "inode_data_mutex.go",
        "inode_io_mutex.go",
        "inode_maps_mutex.go",
        "inode_refs.go",
        "read_write.go",
        "register.go",
        "mmap.go",
        "regular_file.go",
        "request_list.go",
        "request_response.go",
        "save_restore.go",
        "seqatomic_time_unsafe.go",
    ],
    marshal = True,
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/marshal",
        "//pkg/marshal/primitive",
        "//pkg/refs",
        "//pkg/safemem",
        "//pkg/sentry/arch",
        "//pkg/sentry/fsimpl/kernfs",
        "//pkg/sentry/fsutil",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/kernel/pipe",
        "//pkg/sentry/ktime",
        "//pkg/sentry/memmap",
        "//pkg/sentry/usage",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/vfs",
        "//pkg/sync",
        "//pkg/sync/locking",
        "//pkg/syserr",
        "//pkg/usermem",
        "//pkg/waiter",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "fuse_test",
    size = "small",
    srcs = [
        "connection_test.go",
        "dev_test.go",
        "host_connection_integration_test.go",
        "host_connection_test.go",
        "utils_test.go",
        "xattr_test.go",
    ],
    library = ":fuse",
    deps = [
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/fspath",
        "//pkg/hostarch",
        "//pkg/marshal/primitive",
        "//pkg/sentry/fsimpl/testutil",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/vfs",
        "//pkg/usermem",
        "//pkg/waiter",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
