package com.gantoine.muxer

import com.gantoine.muxer.images.RemoteImageLimits
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class RemoteImageLimitsTest {
    @Test
    fun normalImagesKeepEnoughResolutionForTheRequestedEdge() {
        assertEquals(1, RemoteImageLimits.sampleSize(1920, 1080, 1080))
        assertEquals(4, RemoteImageLimits.sampleSize(1920, 1080, 256))
        assertEquals(4, RemoteImageLimits.sampleSize(1080, 1920, 256))
    }

    @Test
    fun thinImagesAreSampledByTheirLongEdge() {
        assertEquals(64, RemoteImageLimits.sampleSize(32_768, 1, 256))
        assertEquals(64, RemoteImageLimits.sampleSize(1, 32_768, 256))
        assertEquals(32_768, RemoteImageLimits.sampleSize(65_536, 1, 1))
    }

    @Test
    fun pixelBudgetStillAppliesWhenTheRequestedEdgeIsLarge() {
        assertEquals(2, RemoteImageLimits.sampleSize(4096, 4096, Int.MAX_VALUE))
        assertEquals(8, RemoteImageLimits.sampleSize(10_240, 10_240, Int.MAX_VALUE))
    }

    @Test
    fun roundedUpDimensionsCannotExceedTheEdgeBudget() {
        assertEquals(4, RemoteImageLimits.sampleSize(8193, 1, 2048))
    }

    @Test
    fun invalidOrExcessiveSourceDimensionsAreRejected() {
        for ((width, height) in listOf(0 to 1, 1 to 0, -1 to 100, 100 to -1, 65_537 to 1, 1 to 65_537, Int.MAX_VALUE to Int.MAX_VALUE, 10_241 to 10_240)) {
            assertNull(RemoteImageLimits.sampleSize(width, height, 256))
        }
        assertNull(RemoteImageLimits.sampleSize(100, 100, 0))
        assertNull(RemoteImageLimits.sampleSize(100, 100, -1))
    }
}
