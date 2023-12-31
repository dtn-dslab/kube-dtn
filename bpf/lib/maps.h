#include "sockops.h"
#include <bpf/bpf_helpers.h>

/* when active establish, record local addr as key and remote addr as value
|--------------------------------------------------------------------|
|   key(local ip, local port)   |     Val(remote ip, remoteport)     |
|--------------------------------------------------------------------|
|        A-ip,A-app-port        |    B-cluster-ip,B-cluster-port     |
|--------------------------------------------------------------------|
|       A-ip,A-envoy-port       |              B-ip,B-port           |
|--------------------------------------------------------------------|
*/
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, SOCKOPS_MAP_SIZE);
    __type(key, struct addr_2_tuple);
    __type(value, struct addr_2_tuple);
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} map_active_estab SEC(".maps");

/* This is a proxy map to store current socket 4-tuple and other side socket 4-tuple
|-------------------------------------------------------------------------------------------|
|          key(current socket 4-tuple)        |        Val(other side socket 4-tuple)       |
|-------------------------------------------------------------------------------------------|
| A-ip,A-app-port,B-cluster-ip,B-cluster-port |    127.0.0.1,A-outbound,A-ip:A-app-port     |
|-------------------------------------------------------------------------------------------|
|   127.0.0.1,A-outbound,A-ip:A-app-port      | A-ip:A-app-port,B-cluster-ip,B-cluster-port |
|-------------------------------------------------------------------------------------------|
*/

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, SOCKOPS_MAP_SIZE);
    __type(key, struct socket_4_tuple);
    __type(value, struct socket_4_tuple_extended);
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} map_proxy SEC(".maps");

/* This a sockhash map for sk_msg redirect
|------------------------------------------------------------------------|
|  key(local_ip:local_port, remote_ip:remote_port) |     Val(skops)      |
|------------------------------------------------------------------------|
|   A-ip:A-app-port, B-cluster-ip,B-cluster-port   |     A-app-skops     |    <--- A-app active_estab CB
|------------------------------------------------------------------------|
|          A-ip:A-envoy-port, B-ip:B-port          |    A-envoy-skops    |    <--- A-envoy active_estab CB
|------------------------------------------------------------------------|
|       127.0.0.1:A-outbound, A-ip:A-app-port      |   A-outbound-skops  |    <--- A-outbound passive_estab CB
|------------------------------------------------------------------------|
|        B-ip:B-inbound, A-ip:A-envoy-port         |   B-inbound-skops   |    <--- B-inbound passive_estab CB
|------------------------------------------------------------------------|
*/
struct {
    __uint(type, BPF_MAP_TYPE_SOCKHASH);
    __uint(max_entries, SOCKOPS_MAP_SIZE);
    __uint(key_size, sizeof(struct socket_4_tuple));
    __uint(value_size, sizeof(uint32_t));
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} map_redir SEC(".maps");

/* This a array map for debug configuration and record bypassed packet number
|-----------|------------------------------------|
|     0     |   0/1 (disable/enable debug info)  |
|-----------|------------------------------------|
|     1     |       bypassed packets number      |
|------------------------------------------------|
*/
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 2);
    __type(key, uint32_t);
    __type(value, uint32_t);
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} debug_map SEC(".maps");