package com.example.store;

import java.util.List;
import java.util.*;
import static java.lang.Math.max;
import com.example.base.BaseRepo;

/**
 * Repository for users.
 */
@Service
public class UserRepo extends BaseRepo<User> implements Repo, AutoCloseable {
    private static final int LIMIT = 10;
    protected List<User> users;
    String name;

    public UserRepo(List<User> users) {
        super();
        this.users = users;
    }

    @Override
    public User find(String id) {
        Helper.check(id);
        User u = new User(id);
        return users.get(max(0, LIMIT));
    }

    @Deprecated
    static void legacy() {}

    private abstract static class Inner implements Runnable {}

    public enum Mode { READ, WRITE }

    interface Callback {
        void done(int code);
    }

    public record Pair(String a, int b) {}
}
