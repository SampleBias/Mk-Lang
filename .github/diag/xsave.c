#include <stdio.h>
#include <intrin.h>
#include <immintrin.h>

int main(void) {
    int r[4];
    __cpuidex(r, 0xD, 0);
    unsigned long long xcr0 = _xgetbv(0);
    printf("XCR0=%#llx xsave_enabled_size=%d xsave_max_size=%d\n", xcr0, r[1], r[2]);
    __cpuidex(r, 0xD, 1);
    printf("leaf D.1 eax=%#x (xsaves/xfd bits) ebx(xss size)=%d ecx=%#x\n", r[0], r[1], r[2]);
    __cpuidex(r, 7, 0);
    printf("leaf7.0 ebx=%#x ecx=%#x edx=%#x\n", r[1], r[2], r[3]);
    __cpuidex(r, 7, 1);
    printf("leaf7.1 eax=%#x edx=%#x\n", r[0], r[3]);
    return 0;
}
