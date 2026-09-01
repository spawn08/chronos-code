import kotlin.math.sqrt

interface Shape {
    fun area(): Double
}

class Circle(val radius: Double) : Shape {
    override fun area(): Double = Math.PI * radius * radius
}

fun distance(x: Double, y: Double): Double {
    return sqrt(x * x + y * y)
}
