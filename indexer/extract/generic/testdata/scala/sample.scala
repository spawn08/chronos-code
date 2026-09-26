package com.example.store

import scala.collection.mutable
import com.example.base.{BaseRepo, Repo => R}
import com.example.util._

/** A user repository. */
class UserRepo(db: Database) extends BaseRepo[User] with Repo with AutoCloseable {
  private val cache = mutable.Map[String, User]()
  protected var count: Int = 0

  override def find(id: String): Option[User] = {
    Helper.check(id)
    val u = new User(id)
    cache.get(id)
  }

  @deprecated("old", "1.0")
  def legacy(): Unit = ()
}

trait Repo {
  def find(id: String): Option[User]
}

case class User(id: String)

object UserRepo {
  val Limit = 10
  def apply(db: Database): UserRepo = new UserRepo(db)
}

type Id = String

def topLevel(): Unit = println("x")
