package com.example.store

import kotlin.collections.List
import com.example.base.*
import com.example.util.Helper as H

/** A user repository. */
@Service
class UserRepo(private val db: Database) : BaseRepo<User>(), Repo, AutoCloseable {
    private val cache = mutableMapOf<String, User>()
    internal var count: Int = 0

    override fun find(id: String): User? {
        H.check(id)
        val u = User(id)
        return cache[id] ?: db.load(id)
    }

    @Deprecated("old")
    suspend fun legacy() {}

    companion object {
        const val LIMIT = 10
        fun create(): UserRepo = UserRepo(Database())
    }
}

interface Repo {
    fun find(id: String): User?
}

data class User(val id: String)

enum class Mode { READ, WRITE }

object Registry {
    fun register(r: Repo) {}
}

typealias Id = String

fun topLevel(x: Int): Int = helper(x)

private fun helper(x: Int) = x + 1
