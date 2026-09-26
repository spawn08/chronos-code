require "json"
require_relative "lib/base"

module Store
  # A user repository.
  class UserRepo < BaseRepo
    include Enumerable
    LIMIT = 10

    attr_reader :name

    def initialize(name)
      super()
      @name = name
    end

    def find(id)
      Helper.check(id)
      u = User.new(id)
      helper(id)
    end

    def self.create
      new("x")
    end

    private

    def reset
      @items.clear
    end
  end
end

def top_level(x)
  x + 1
end

class Widget
  private def secret
    1
  end
end
