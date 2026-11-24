package com.gantoine.muxer.images

internal object RemoteImageLimits {
    const val MAX_BYTES = 5 * 1024 * 1024
    // Allow line-wrapped Base64 while bounding allocation before decoding.
    const val MAX_ENCODED_CHARS = 8 * 1024 * 1024
    private const val MAX_SOURCE_EDGE = 65_536
    private const val MAX_SOURCE_PIXELS = 100L * 1024 * 1024
    private const val MAX_DECODED_EDGE = 4096
    private const val MAX_DECODED_PIXELS = 4L * 1024 * 1024

    fun sampleSize(width: Int, height: Int, edge: Int): Int? {
        if (width !in 1..MAX_SOURCE_EDGE || height !in 1..MAX_SOURCE_EDGE || edge <= 0) return null
        if (width.toLong() * height > MAX_SOURCE_PIXELS) return null
        // Power-of-two sampling may retain up to twice the requested edge.
        val maxEdge = edge.coerceAtMost(MAX_DECODED_EDGE / 2) * 2
        var sample = 1
        while (true) {
            val sampledWidth = (width.toLong() + sample - 1) / sample
            val sampledHeight = (height.toLong() + sample - 1) / sample
            if (maxOf(sampledWidth, sampledHeight) <= maxEdge && sampledWidth * sampledHeight <= MAX_DECODED_PIXELS) return sample
            sample *= 2
        }
    }
}
